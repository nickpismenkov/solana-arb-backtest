//! Verify the shred swap decoder against real on-chain swaps. Pulls recent
//! signatures for our pools, fetches each tx, resolves ALTs, and decodes the
//! swaps — so we confirm direction/amount extraction (incl. ALT-referenced
//! swaps) before wiring the decoder into the shred-time pricer.
//!
//! Usage: RPC_ENDPOINT=https://api.mainnet-beta.solana.com cargo run --bin decode_probe

use arb_engine::decode::{decode_swaps, AltCache};
use arb_engine::pools::pair;
use base64::Engine;
use solana_transaction::versioned::VersionedTransaction;

fn rpc(endpoint: &str, method: &str, params: serde_json::Value) -> Option<serde_json::Value> {
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":method,"params":params});
    ureq::post(endpoint).send_json(body).ok()?.into_json().ok()
}

fn recent_sigs(endpoint: &str, pool: &str, limit: u64) -> Vec<String> {
    let r = rpc(
        endpoint,
        "getSignaturesForAddress",
        serde_json::json!([pool, {"limit": limit}]),
    );
    r.and_then(|v| v["result"].as_array().cloned())
        .unwrap_or_default()
        .iter()
        .filter_map(|s| s["signature"].as_str().map(String::from))
        .collect()
}

fn fetch_tx(endpoint: &str, sig: &str) -> Option<VersionedTransaction> {
    let r = rpc(
        endpoint,
        "getTransaction",
        serde_json::json!([sig, {"encoding":"base64","maxSupportedTransactionVersion":0}]),
    )?;
    let b64 = r["result"]["transaction"][0].as_str()?;
    let bytes = base64::engine::general_purpose::STANDARD.decode(b64).ok()?;
    bincode::deserialize::<VersionedTransaction>(&bytes).ok()
}

fn main() {
    let endpoint = std::env::var("RPC_ENDPOINT")
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());
    let mut alt = AltCache::new(&endpoint);

    let (mut n_tx, mut n_swaps, mut n_alt) = (0u32, 0u32, 0u32);
    for (label, pool) in [("Orca", pair().orca_pool.as_str()), ("Raydium", pair().ray_pool.as_str())] {
        println!("\n=== {label} pool {pool} — recent swaps ===");
        for sig in recent_sigs(&endpoint, pool, 8) {
            let Some(txn) = fetch_tx(&endpoint, &sig) else {
                continue;
            };
            n_tx += 1;
            let uses_alt = matches!(&txn.message, solana_message::VersionedMessage::V0(m) if !m.address_table_lookups.is_empty());
            if uses_alt {
                n_alt += 1;
            }
            let Some(keys) = alt.resolve_keys(&txn.message) else {
                println!("  {}… ALT resolve failed", &sig[..8]);
                continue;
            };
            let swaps = decode_swaps(&txn, &keys);
            for s in &swaps {
                n_swaps += 1;
                println!(
                    "  {}… {:>7} {:?} amount={} input={} alt={}",
                    &sig[..8],
                    s.kind,
                    s.dir,
                    s.amount,
                    s.amount_is_input,
                    uses_alt
                );
            }
            if swaps.is_empty() {
                // Diagnose: is the pool present in resolved keys (ALT ok) and
                // what top-level programs are calling it (CPI/router)?
                let pool_pk = solana_pubkey::Pubkey::from_str_const(pool);
                let pool_in_keys = keys.contains(&pool_pk);
                let progs: Vec<String> = match &txn.message {
                    solana_message::VersionedMessage::V0(m) => m
                        .instructions
                        .iter()
                        .filter_map(|ix| keys.get(ix.program_id_index as usize))
                        .map(|p| p.to_string()[..8].to_string())
                        .collect(),
                    _ => vec![],
                };
                println!(
                    "  {}… no swap; pool_in_resolved_keys={pool_in_keys} top_level_programs={progs:?}",
                    &sig[..8]
                );
            }
        }
    }
    println!("\ntxs fetched={n_tx}  swaps decoded={n_swaps}  txs using ALTs={n_alt}");
}
