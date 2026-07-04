//! arb-engine — Solana cross-venue arb shadow/measurement engine (Rust).
//! Shared modules used by the `shadow` and `shred_probe` binaries.

pub mod arb;
pub mod decode;
pub mod observe;
pub mod detector;
pub mod execute;
pub mod flashloan;
pub mod grpc;
pub mod jito;
pub mod marginfi;
pub mod pools;
pub mod shredstream;
pub mod signal;
pub mod swap;
