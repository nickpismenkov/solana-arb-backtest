//! Profitability watcher — proves the WINNING path with zero cost. Loops over
//! live mainnet: refresh both pools, build the guarded arb (both directions)
//! from fresh state, simulateTransaction. Most iterations show the guard
//! reverting at leg 2 (no edge). The instant a real edge appears — standing or
//! transient — the sim comes back clean (err=null), meaning a profitable arb
//! exists right now and our tx would land. Logs every clean hit + the spot edge
//! to profit_watch.jsonl. No money, no submission — pure measurement.
//!
//! Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> [BORROW_USDC=500] [POLL_MS=800] \
//!   [RUN_DIR=runs] cargo run --release --bin profit_watch

use arb_engine::arb::{build_arb_tx, load_alt, PoolData};
use arb_engine::pools::{orca_price, pair, ray_clmm_price};
use base64::Engine;
use solana_hash::Hash;
use solana_pubkey::Pubkey;
use std::fs::OpenOptions;
use std::io::Write;
use std::str::FromStr;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..3 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(Duration::from_millis(200 << attempt));
    }
    None
}

fn account_data(endpoint: &str, addr: &str) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[addr,{"encoding":"base64"}]}))?;
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"][0].as_str()?).ok()
}

fn now() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let alt_addr = std::env::var("ALT_ADDRESS").expect("ALT_ADDRESS");
    let borrow_ui: f64 = std::env::var("BORROW_USDC").ok().and_then(|s| s.parse().ok()).unwrap_or(500.0);
    let poll_ms: u64 = std::env::var("POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(800);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let borrow_amount = (borrow_ui * 1e6) as u64;
    let cfg = pair();
    // Placeholder signer — simulate with sigVerify=false, so no keypair needed.
    let signer = Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap();

    let alt = load_alt(&alt_addr, &account_data(&endpoint, &alt_addr).expect("ALT"));
    let _ = std::fs::create_dir_all(&run_dir);
    let out = format!("{run_dir}/profit_watch.jsonl");

    eprintln!("profit-watch {} borrow {borrow_ui} USDC poll {poll_ms}ms — simulating both dirs; logs clean hits → {out}", cfg.label);
    let (mut iters, mut clean, mut best_edge_bps) = (0u64, 0u64, f64::MIN);

    loop {
        iters += 1;
        let (Some(o), Some(r)) = (account_data(&endpoint, &cfg.orca_pool), account_data(&endpoint, &cfg.ray_pool)) else {
            std::thread::sleep(Duration::from_millis(poll_ms));
            continue;
        };
        // Spot edge for context (stale-free here: just-fetched pools).
        let edge_bps = match (orca_price(&o), ray_clmm_price(&r)) {
            (Some(po), Some(pr)) if po > 0.0 && pr > 0.0 => {
                ((pr - po).abs() / po.min(pr)) * 1e4 - cfg.round_trip_fee_bps()
            }
            _ => f64::NAN,
        };
        if edge_bps.is_finite() && edge_bps > best_edge_bps {
            best_edge_bps = edge_bps;
        }
        let pools = PoolData { orca: o, ray: r };
        let bh = Hash::default();

        for orca_first in [false, true] {
            let Ok(tx) = build_arb_tx(&pools, signer, &alt, borrow_amount, orca_first, None, 0, 10_000, bh) else { continue };
            let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
            let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
                "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}));
            let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
            if val["err"].is_null() && !val.is_null() {
                clean += 1;
                let dir = if orca_first { "orca→ray" } else { "ray→orca" };
                let cu = val["unitsConsumed"].clone();
                eprintln!("🎉 CLEAN SIM [{dir}] — profitable arb exists NOW, tx would land (edge≈{edge_bps:.2}bp, cu={cu})");
                let row = serde_json::json!({"t": now(), "dir": dir, "edge_bps": edge_bps, "cu": cu, "borrow_usdc": borrow_ui});
                if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(&out) {
                    let _ = writeln!(f, "{row}");
                }
            }
        }
        if iters % 50 == 0 {
            eprintln!("[profit-watch] iters={iters} clean_sims={clean} best_edge={best_edge_bps:.2}bp (need >0 to profit)");
        }
        std::thread::sleep(Duration::from_millis(poll_ms));
    }
}
