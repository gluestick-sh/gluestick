//! `shim.exe` — a thin wrapper around the [`gluestick_shim`] library so hosts
//! that ship the shim beside their own executable (the desktop client) can
//! build it from the library instead of reaching into this crate's sources.
//!
//! All launcher logic lives in `gluestick_shim::run_or_exit`.

fn main() {
    gluestick_shim::run_or_exit();
}
