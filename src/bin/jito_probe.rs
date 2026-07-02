//! Connectivity check for the Jito block engine (safe — read-only, no bundle
//! sent, no money). Confirms we can reach the region-matched block engine and
//! fetch tip accounts before wiring live submission.
//!
//! Usage: JITO_BLOCK_ENGINE=https://amsterdam.mainnet.block-engine.jito.wtf \
//!          cargo run --release --bin jito_probe

fn main() {
    let _ = dotenvy::dotenv();
    let be = arb_engine::jito::default_block_engine();
    println!("block engine: {be}");
    match arb_engine::jito::get_tip_accounts(&be) {
        Ok(accts) => {
            println!("reachable ✓  {} tip accounts:", accts.len());
            for a in &accts {
                println!("  {a}");
            }
        }
        Err(e) => println!("FAILED: {e:#}"),
    }
}
