//! Cooperative cancellation for long-running operations.
//!
//! An install or update carries a [`CancelToken`] into every phase and into
//! each download it starts. Any clone of the token can flip the flag; every
//! long loop (download reads, chunk workers, phase boundaries) polls it.
//!
//! Cancelling is not a failure the user should see as one: the engine reports
//! [`InstallError::Cancelled`](crate::engine::install::InstallError), so a UI
//! can tell "paused by the user" from "broke", and whatever is worth keeping
//! stays on disk for the retry — artifacts already in the content store are
//! reused, and a resumable `.part` file is left for the next attempt.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

/// A cloneable "stop asking for bytes" flag.
///
/// Clones share one flag; the token is cheap enough to hand to every worker
/// thread and every download task.
#[derive(Debug, Clone, Default)]
pub struct CancelToken(Arc<AtomicBool>);

impl CancelToken {
    pub fn new() -> CancelToken {
        CancelToken(Arc::new(AtomicBool::new(false)))
    }

    /// Ask every holder to stop. Idempotent.
    pub fn cancel(&self) {
        self.0.store(true, Ordering::SeqCst);
    }

    pub fn is_cancelled(&self) -> bool {
        self.0.load(Ordering::SeqCst)
    }
}

impl PartialEq for CancelToken {
    /// Two tokens are equal when they share the same flag: a download `Task`
    /// compares its cancellation source the same way it compares the rest of
    /// its fields, and a fresh, independent token is never "equal".
    fn eq(&self, other: &CancelToken) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }
}

impl Eq for CancelToken {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clones_share_the_flag() {
        let token = CancelToken::new();
        let clone = token.clone();
        assert!(!clone.is_cancelled());
        token.cancel();
        assert!(clone.is_cancelled());
        assert_eq!(token, clone);
        assert_ne!(CancelToken::new(), CancelToken::new());
    }
}
