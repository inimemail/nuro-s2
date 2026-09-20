//! Retry delays own no callback worker. A bounded ready/delayed set is shared
//! by live callbacks and WAL replay; due retries join the FIFO ready queue.
use futures_util::{stream::FuturesUnordered, StreamExt};
use std::{
    collections::{BTreeMap, VecDeque},
    future::Future,
    time::Duration,
};
use tokio::{sync::mpsc, time::Instant};

pub(crate) async fn run<T, F, Fut, D, Deferred>(
    mut receiver: mpsc::Receiver<T>,
    concurrency: usize,
    capacity: usize,
    attempt: F,
    defer: D,
) where
    F: Fn(T) -> Fut,
    Fut: Future<Output = Option<(Duration, T)>>,
    D: Fn(Duration, T) -> Deferred,
    Deferred: Future<Output = ()>,
{
    let mut ready = VecDeque::new();
    let mut delayed = BTreeMap::new();
    let mut active = FuturesUnordered::new();
    let mut spilling = FuturesUnordered::new();
    let mut sequence = 0_u64;
    let mut closed = false;
    loop {
        while active.len() + spilling.len() < concurrency {
            let Some(task) = ready.pop_front() else {
                break;
            };
            active.push(Box::pin(attempt(task)));
        }
        if closed
            && active.is_empty()
            && spilling.is_empty()
            && ready.is_empty()
            && delayed.is_empty()
        {
            return;
        }
        let next = delayed.first_key_value().map(|((when, _), _)| *when);
        tokio::select! {
            result = active.next(), if !active.is_empty() => {
                if let Some(Some((delay, task))) = result {
                    if delayed.len() < capacity {
                        sequence = sequence.wrapping_add(1);
                        delayed.insert((Instant::now() + delay, sequence), task);
                    } else {
                        // Disk-backed overflow preserves failed work without
                        // allowing sleeping retries to block fresh admission.
                        spilling.push(Box::pin(defer(delay, task)));
                    }
                }
            }
            _ = spilling.next(), if !spilling.is_empty() => {}
            task = receiver.recv(), if !closed && ready.len() < concurrency => {
                match task { Some(task) => ready.push_back(task), None => closed = true }
            }
            _ = async {
                match next {
                    Some(when) => tokio::time::sleep_until(when).await,
                    None => std::future::pending().await,
                }
            }, if next.is_some() && ready.len() < concurrency => {
                if let Some((_, task)) = delayed.pop_first() {
                    ready.push_back(task);
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        Arc,
    };

    #[tokio::test]
    async fn full_delayed_queue_still_admits_live_work_and_preserves_overflow() {
        let (tx, rx) = mpsc::channel(8);
        let (attempted, mut attempts) = mpsc::unbounded_channel();
        let (deferred, mut overflow) = mpsc::unbounded_channel();
        let runner = tokio::spawn(run(
            rx,
            1,
            2,
            move |id| {
                let attempted = attempted.clone();
                async move {
                    attempted.send(id).unwrap();
                    if id < 3 {
                        Some((Duration::from_secs(30), id))
                    } else {
                        None
                    }
                }
            },
            move |delay, id| {
                let deferred = deferred.clone();
                async move {
                    deferred.send((delay, id)).unwrap();
                }
            },
        ));
        for id in 0..3 {
            tx.send(id).await.unwrap();
            assert_eq!(
                tokio::time::timeout(Duration::from_secs(1), attempts.recv())
                    .await
                    .unwrap(),
                Some(id)
            );
        }
        assert_eq!(
            tokio::time::timeout(Duration::from_secs(1), overflow.recv())
                .await
                .unwrap(),
            Some((Duration::from_secs(30), 2))
        );
        tx.send(100).await.unwrap();
        assert_eq!(
            tokio::time::timeout(Duration::from_secs(1), attempts.recv())
                .await
                .unwrap(),
            Some(100)
        );
        runner.abort();
        assert!(runner.await.unwrap_err().is_cancelled());
    }

    #[tokio::test]
    async fn failed_history_releases_all_workers_for_new_callbacks() {
        let (tx, rx) = mpsc::channel(128);
        let (events, mut observed) = mpsc::unbounded_channel();
        let active = Arc::new(AtomicUsize::new(0));
        let max_active = Arc::new(AtomicUsize::new(0));
        let count = active.clone();
        let peak = max_active.clone();
        let runner = tokio::spawn(run(
            rx,
            32,
            128,
            move |(id, attempts)| {
                let events = events.clone();
                let active = count.clone();
                let peak = peak.clone();
                async move {
                    let now = active.fetch_add(1, Ordering::SeqCst) + 1;
                    peak.fetch_max(now, Ordering::SeqCst);
                    tokio::task::yield_now().await;
                    active.fetch_sub(1, Ordering::SeqCst);
                    events.send((id, attempts)).unwrap();
                    if id < 64 && attempts < 2 {
                        Some((Duration::from_millis(200), (id, attempts + 1)))
                    } else {
                        None
                    }
                }
            },
            |_, _| async {
                panic!("unexpected overflow");
            },
        ));
        for id in 0..64 {
            tx.send((id, 0)).await.unwrap();
        }
        for _ in 0..64 {
            let (_, attempt) = tokio::time::timeout(Duration::from_secs(2), observed.recv())
                .await
                .unwrap()
                .unwrap();
            assert_eq!(attempt, 0);
        }
        assert_eq!(active.load(Ordering::SeqCst), 0);
        tx.send((100, 0)).await.unwrap();
        assert_eq!(
            tokio::time::timeout(Duration::from_millis(100), observed.recv())
                .await
                .unwrap(),
            Some((100, 0))
        );
        let mut retries = 0;
        while retries < 128 {
            let (id, attempt) = tokio::time::timeout(Duration::from_secs(2), observed.recv())
                .await
                .unwrap()
                .unwrap();
            assert!(id < 64 && attempt > 0);
            retries += 1;
        }
        assert!(max_active.load(Ordering::SeqCst) <= 32);
        drop(tx);
        runner.await.unwrap();
    }
}
