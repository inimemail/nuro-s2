//! Bounded disk ownership. No filesystem operation or disk lock runs on a
//! Tokio worker. Cleanup reservations are acquired while callers can await,
//! so Drop can hand off durable work without blocking or losing it to a full
//! queue. Ordinary commands have extra capacity to avoid reservation deadlock.
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc,
};
use tokio::sync::{mpsc, oneshot, OwnedSemaphorePermit, Semaphore};

type Operation<T> = Box<dyn FnOnce(&T) + Send>;

struct Command<T> {
    operation: Option<Operation<T>>,
    pending: Arc<AtomicU64>,
    _reservation: Option<OwnedSemaphorePermit>,
}

impl<T> Drop for Command<T> {
    fn drop(&mut self) {
        self.pending.fetch_sub(1, Ordering::AcqRel);
    }
}

pub(crate) struct DiskWorker<T> {
    pub(crate) data: Arc<T>,
    sender: mpsc::Sender<Command<T>>,
    cleanup_slots: [Arc<Semaphore>; 4],
    pub(crate) pending: Arc<AtomicU64>,
}

pub(crate) struct CleanupReservation<T> {
    permit: mpsc::OwnedPermit<Command<T>>,
    slot: OwnedSemaphorePermit,
    pending: Arc<AtomicU64>,
}

impl<T: Send + Sync + 'static> DiskWorker<T> {
    pub(crate) async fn start(
        ordinary_capacity: usize,
        cleanup_capacity: usize,
        init: impl FnOnce() -> anyhow::Result<T> + Send + 'static,
    ) -> anyhow::Result<Self> {
        let (sender, mut receiver) =
            mpsc::channel::<Command<T>>(ordinary_capacity + cleanup_capacity * 4);
        let (ready_tx, ready_rx) = oneshot::channel();
        std::thread::Builder::new()
            .name("settlement-wal".into())
            .spawn(move || {
                let data = match init() {
                    Ok(data) => Arc::new(data),
                    Err(error) => {
                        let _ = ready_tx.send(Err(error));
                        return;
                    }
                };
                if ready_tx.send(Ok(data.clone())).is_err() {
                    return;
                }
                while let Some(mut command) = receiver.blocking_recv() {
                    if let Some(operation) = command.operation.take() {
                        operation(&data);
                    }
                }
            })?;
        let data = ready_rx.await??;
        Ok(Self {
            data,
            sender,
            cleanup_slots: std::array::from_fn(|_| Arc::new(Semaphore::new(cleanup_capacity))),
            pending: Arc::new(AtomicU64::new(0)),
        })
    }

    pub(crate) async fn run<R: Send + 'static>(
        &self,
        operation: impl FnOnce(&T) -> R + Send + 'static,
    ) -> anyhow::Result<R> {
        let permit = self.sender.reserve().await?;
        let (tx, rx) = oneshot::channel();
        self.pending.fetch_add(1, Ordering::Release);
        permit.send(Command {
            operation: Some(Box::new(move |data| {
                let _ = tx.send(operation(data));
            })),
            pending: self.pending.clone(),
            _reservation: None,
        });
        Ok(rx.await?)
    }

    pub(crate) async fn reserve_cleanup(&self, stage: usize) -> Option<CleanupReservation<T>> {
        // Nested guards must not compete with their parents for the same
        // reservation pool: saturated parents could otherwise prevent every
        // child from progressing. Stages follow prepare -> relay -> outer -> inner.
        let slot = self.cleanup_slots[stage]
            .clone()
            .acquire_owned()
            .await
            .ok()?;
        let permit = self.sender.clone().reserve_owned().await.ok()?;
        Some(CleanupReservation {
            permit,
            slot,
            pending: self.pending.clone(),
        })
    }

    /// Only for the main thread AFTER the Tokio runtime has stopped and
    /// dropped its tasks. Their Drop handlers may have queued final usage.
    pub(crate) fn finish<R: Send + 'static>(
        &self,
        operation: impl FnOnce(&T) -> R + Send + 'static,
    ) -> anyhow::Result<R> {
        let (tx, rx) = oneshot::channel();
        self.pending.fetch_add(1, Ordering::Release);
        self.sender
            .blocking_send(Command {
                operation: Some(Box::new(move |data| {
                    let _ = tx.send(operation(data));
                })),
                pending: self.pending.clone(),
                _reservation: None,
            })
            .map_err(|_| anyhow::anyhow!("disk worker stopped before final flush"))?;
        Ok(rx.blocking_recv()?)
    }
}

impl<T> CleanupReservation<T> {
    pub(crate) fn send(self, operation: impl FnOnce(&T) + Send + 'static) {
        self.pending.fetch_add(1, Ordering::Release);
        self.permit.send(Command {
            operation: Some(Box::new(operation)),
            pending: self.pending,
            _reservation: Some(self.slot),
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, Instant};

    #[tokio::test]
    async fn slow_disk_does_not_block_current_thread_timers() {
        let worker = Arc::new(DiskWorker::start(1, 1, || Ok(())).await.unwrap());
        let (release, blocked) = std::sync::mpsc::channel();
        let (entered, started) = oneshot::channel();
        let other = worker.clone();
        let before = Instant::now();
        let disk = tokio::spawn(async move {
            other
                .run(move |_| {
                    let _ = entered.send(());
                    let _ = blocked.recv_timeout(Duration::from_secs(2));
                })
                .await
                .unwrap()
        });
        started.await.unwrap();
        tokio::time::sleep(Duration::from_millis(10)).await;
        assert!(
            before.elapsed() < Duration::from_secs(1),
            "disk blocked the runtime"
        );
        release.send(()).unwrap();
        disk.await.unwrap();
    }

    #[tokio::test]
    async fn reserved_cleanup_survives_full_queue_without_blocking_drop() {
        let worker = Arc::new(DiskWorker::start(1, 1, || Ok(())).await.unwrap());
        let reserved = worker.reserve_cleanup(0).await.unwrap();
        let other_stages = [
            worker.reserve_cleanup(1).await.unwrap(),
            worker.reserve_cleanup(2).await.unwrap(),
            worker.reserve_cleanup(3).await.unwrap(),
        ];
        let (release, blocked) = std::sync::mpsc::channel();
        let (entered, started) = oneshot::channel();
        let other = worker.clone();
        let disk = tokio::spawn(async move {
            other
                .run(move |_| {
                    let _ = entered.send(());
                    let _ = blocked.recv_timeout(Duration::from_secs(2));
                })
                .await
                .unwrap()
        });
        started.await.unwrap();
        let other = worker.clone();
        let normal = tokio::spawn(async move { other.run(|_| 7).await.unwrap() });
        while worker.sender.capacity() != 0 {
            tokio::task::yield_now().await;
        }
        let (done, completed) = oneshot::channel();
        // All channel capacity is occupied/reserved, but Drop's handoff cannot fail.
        reserved.send(move |_| {
            let _ = done.send(());
        });
        assert_eq!(worker.pending.load(Ordering::Acquire), 3);
        release.send(()).unwrap();
        disk.await.unwrap();
        assert_eq!(normal.await.unwrap(), 7);
        completed.await.unwrap();
        worker.run(|_| ()).await.unwrap();
        assert_eq!(worker.cleanup_slots[0].available_permits(), 1);
        drop(other_stages);
    }

    #[tokio::test]
    async fn all_cleanup_slots_reserved_leave_room_for_normal_flush() {
        let worker = DiskWorker::start(1, 2, || Ok(())).await.unwrap();
        let first = worker.reserve_cleanup(0).await.unwrap();
        let second = worker.reserve_cleanup(0).await.unwrap();
        tokio::time::timeout(Duration::from_secs(1), worker.run(|_| ()))
            .await
            .unwrap()
            .unwrap();
        drop((first, second));
        assert_eq!(worker.cleanup_slots[0].available_permits(), 2);
    }

    #[tokio::test]
    async fn saturated_parent_guards_cannot_deadlock_nested_cleanup() {
        let worker = DiskWorker::start(1, 1, || Ok(())).await.unwrap();
        let parent = worker.reserve_cleanup(0).await.unwrap();
        let child = tokio::time::timeout(Duration::from_secs(1), worker.reserve_cleanup(1))
            .await
            .unwrap()
            .unwrap();
        let outer = tokio::time::timeout(Duration::from_secs(1), worker.reserve_cleanup(2))
            .await
            .unwrap()
            .unwrap();
        let inner = tokio::time::timeout(Duration::from_secs(1), worker.reserve_cleanup(3))
            .await
            .unwrap()
            .unwrap();
        tokio::time::timeout(Duration::from_secs(1), worker.run(|_| ()))
            .await
            .unwrap()
            .unwrap();
        drop((parent, child, outer, inner));
    }
}
