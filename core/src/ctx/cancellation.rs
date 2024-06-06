#![cfg(feature = "scripting")]

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
#[cfg(target_arch = "wasm32-unknown-unknown")]
use trice::Instant;
#[cfg(not(target_arch = "wasm32-unknown-unknown"))]
use std::time::Instant;

/// A 'static view into the cancellation status of a Context.
#[derive(Clone, Debug, Default)]
#[non_exhaustive]
pub struct Cancellation {
	deadline: Option<Instant>,
	cancellations: Vec<Arc<AtomicBool>>,
}

impl Cancellation {
	pub fn new(deadline: Option<Instant>, cancellations: Vec<Arc<AtomicBool>>) -> Cancellation {
		Self {
			deadline,
			cancellations,
		}
	}

	pub fn is_done(&self) -> bool {
		self.deadline.map(|d| d <= Instant::now()).unwrap_or(false)
			|| self.cancellations.iter().any(|c| c.load(Ordering::Relaxed))
	}
}
