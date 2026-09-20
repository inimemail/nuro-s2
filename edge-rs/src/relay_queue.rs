//! Queue timeout ends at an atomic admission boundary, never at response
//! headers. A timeout winning that boundary proves no upstream send can start.
use std::{
    sync::{
        atomic::{AtomicU8, Ordering},
        Arc,
    },
    time::{Duration, Instant},
};
use tokio::sync::{oneshot, OwnedSemaphorePermit, Semaphore};

const WAITING: u8 = 0;
const STARTED: u8 = 1;
const CANCELLED: u8 = 2;

pub(crate) struct Admission {
    deadline: Option<tokio::time::Instant>,
    state: AtomicU8,
}

fn timeout_error() -> anyhow::Error {
    anyhow::anyhow!("edge_queue_wait_timeout: queue wait budget exceeded")
}

impl Admission {
    pub(crate) fn new(enqueued_at: Instant, budget_ms: u64) -> Self {
        Self {
            deadline: (budget_ms > 0).then(|| {
                tokio::time::Instant::from_std(enqueued_at) + Duration::from_millis(budget_ms)
            }),
            state: AtomicU8::new(WAITING),
        }
    }

    pub(crate) fn start(&self) -> anyhow::Result<()> {
        if self
            .deadline
            .is_some_and(|when| tokio::time::Instant::now() >= when)
        {
            self.cancel_waiting();
            return Err(timeout_error());
        }
        self.state
            .compare_exchange(WAITING, STARTED, Ordering::AcqRel, Ordering::Acquire)
            .map(|_| ())
            .map_err(|_| timeout_error())
    }

    fn cancel_waiting(&self) -> bool {
        self.state
            .compare_exchange(WAITING, CANCELLED, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
    }

    async fn deadline_elapsed(&self) {
        match self.deadline {
            Some(when) => tokio::time::sleep_until(when).await,
            None => std::future::pending().await,
        }
    }

    pub(crate) async fn acquire<T>(
        &self,
        semaphore: Arc<Semaphore>,
        response: &mut oneshot::Sender<T>,
    ) -> anyhow::Result<OwnedSemaphorePermit> {
        tokio::select! {
            biased;
            _ = response.closed() => anyhow::bail!("edge relay caller cancelled"),
            _ = self.deadline_elapsed() => Err(timeout_error()),
            permit = semaphore.acquire_owned() => Ok(permit?),
        }
    }

    pub(crate) async fn response<T>(
        &self,
        mut receiver: oneshot::Receiver<T>,
    ) -> anyhow::Result<T> {
        tokio::select! {
            result = &mut receiver => return Ok(result?),
            _ = self.deadline_elapsed() => {}
        }
        if self.cancel_waiting() {
            return Err(timeout_error());
        }
        // Admission won the race. Generation may be arbitrarily long; the
        // queue budget must not cancel it or permit a second upstream send.
        Ok(receiver.await?)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn queue_deadline_covers_wait_for_global_permit_and_releases_resources() {
        let semaphore = Arc::new(Semaphore::new(1));
        let held = semaphore.clone().acquire_owned().await.unwrap();
        let admission = Admission::new(Instant::now(), 10);
        let (mut tx, rx) = oneshot::channel::<()>();
        let result = tokio::time::timeout(
            Duration::from_secs(1),
            admission.acquire(semaphore.clone(), &mut tx),
        )
        .await
        .unwrap();
        assert!(result.is_err());
        assert!(admission.response(rx).await.is_err());
        assert!(admission.start().is_err());
        drop(held);
        assert_eq!(semaphore.available_permits(), 1);
    }

    #[tokio::test]
    async fn caller_deadline_cancels_even_before_executor_dequeues() {
        let admission = Admission::new(Instant::now(), 5);
        let (mut tx, rx) = oneshot::channel::<()>();
        assert!(admission.response(rx).await.is_err());
        assert!(tx.is_closed());
        let semaphore = Arc::new(Semaphore::new(1));
        assert!(admission.acquire(semaphore.clone(), &mut tx).await.is_err());
        assert!(admission.start().is_err());
        assert_eq!(semaphore.available_permits(), 1);
    }

    #[tokio::test]
    async fn admitted_generation_outlives_queue_budget() {
        let admission = Admission::new(Instant::now(), 5);
        admission.start().unwrap();
        let (tx, rx) = oneshot::channel();
        let (result, _) = tokio::join!(admission.response(rx), async {
            tokio::time::sleep(Duration::from_millis(30)).await;
            tx.send(7).unwrap();
        });
        assert_eq!(result.unwrap(), 7);
    }

    #[tokio::test]
    async fn cancellation_interrupts_an_unlimited_permit_wait() {
        let admission = Admission::new(Instant::now(), 0);
        let semaphore = Arc::new(Semaphore::new(0));
        let (mut tx, rx) = oneshot::channel::<()>();
        let (result, _) = tokio::join!(admission.acquire(semaphore, &mut tx), async {
            tokio::task::yield_now().await;
            drop(rx);
        });
        assert!(result.is_err());
    }

    #[tokio::test]
    async fn cancellation_and_start_have_exactly_one_winner() {
        for _ in 0..200 {
            let admission = Arc::new(Admission::new(Instant::now(), 0));
            let other = admission.clone();
            let start = tokio::spawn(async move { other.start().is_ok() });
            let cancelled = admission.cancel_waiting();
            assert_ne!(cancelled, start.await.unwrap());
        }
    }
}
