//! Verify the in-memory CLMM math against reality. For a range of USDC sizes,
//! compare our `apply_swap` (USDC→base, per venue) against Jupiter's quote for
//! that exact single-venue swap. Close match = our within-tick math is right
//! and we can trust the profit optimiser. Divergence at larger sizes = where
//! tick-crossing starts to matter (Stage 1b).
//!
//! Then print the current optimal cross-venue arb (size + exact profit) so we
//! can see, live, whether SOL/USDC ever shows a positive number.
//!
//! Usage: RPC_ENDPOINT=<url> cargo run --release --bin clmm_probe

use arb_engine::clmm::{optimal_arb, wsol, ClmmState};
use arb_engine::pools::pair;
use base64::Engine;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(300 << attempt));
    }
    None
}
fn account_data(endpoint: &str, addr: &str) -> Vec<u8> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[addr,{"encoding":"base64"}]})).expect("rpc");
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"][0].as_str().expect("data")).unwrap()
}

const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

/// Jupiter quote: exact-in USDC→SOL restricted to one DEX. Returns SOL out (raw lamports).
fn jup_quote(base_mint: &str, amount_usdc_raw: u64, dex: &str) -> Option<f64> {
    let url = format!(
        "https://lite-api.jup.ag/swap/v1/quote?inputMint={USDC}&outputMint={base_mint}&amount={amount_usdc_raw}&onlyDirectRoutes=true&swapMode=ExactIn&dexes={dex}"
    );
    let resp = match ureq::get(&url).call() {
        Ok(r) => r,
        Err(e) => { eprintln!("  (jup err: {e})"); return None; }
    };
    let v: serde_json::Value = resp.into_json().ok()?;
    v["outAmount"].as_str().and_then(|s| s.parse::<f64>().ok())
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let cfg = pair();
    let base = wsol();

    let od = account_data(&endpoint, &cfg.orca_pool);
    let rd = account_data(&endpoint, &cfg.ray_pool);
    // Orca decimals by mint order: mintA@101.
    let mint_a = arb_engine::clmm::ClmmState::from_orca(&od, 0, 0, cfg.orca_fee_bps).map(|s| s.mint0).unwrap();
    let base_is_a = mint_a == base;
    let (oa_dec0, oa_dec1) = if base_is_a { (cfg.base_dec, cfg.quote_dec) } else { (cfg.quote_dec, cfg.base_dec) };
    let orca = ClmmState::from_orca(&od, oa_dec0, oa_dec1, cfg.orca_fee_bps).unwrap();
    let ray = ClmmState::from_ray(&rd, cfg.ray_fee_bps).unwrap();

    println!("Orca  ui_price={:.4}  L={:.3e}  fee={}bp", orca.ui_price(), orca.liquidity, cfg.orca_fee_bps);
    println!("Ray   ui_price={:.4}  L={:.3e}  fee={}bp", ray.ui_price(), ray.liquidity, cfg.ray_fee_bps);

    println!("\n═══ apply_swap vs Jupiter (USDC→SOL, single venue) ═══");
    for usdc in [100.0, 500.0, 2000.0, 10000.0] {
        let raw = (usdc * 1e6) as u64;
        let usdc_is_0_orca = orca.mint0 != base;
        let ours_orca = orca.apply_swap(usdc_is_0_orca, usdc * 1e6) / 1e9;
        let jup_orca = jup_quote(&cfg.base_mint, raw, "Whirlpool").map(|o| o / 1e9);
        let usdc_is_0_ray = ray.mint0 != base;
        let ours_ray = ray.apply_swap(usdc_is_0_ray, usdc * 1e6) / 1e9;
        let jup_ray = jup_quote(&cfg.base_mint, raw, "Raydium%20CLMM").map(|o| o / 1e9);
        let err = |ours: f64, jup: Option<f64>| jup.map(|j| format!("jup={j:.6} Δ={:+.3}%", 100.0 * (ours - j) / j)).unwrap_or_else(|| "jup=n/a".into());
        println!("  {usdc:>7.0} USDC | Orca ours={ours_orca:.6} {}", err(ours_orca, jup_orca));
        println!("          | Ray  ours={ours_ray:.6} {}", err(ours_ray, jup_ray));
        std::thread::sleep(Duration::from_millis(400));
    }

    println!("\n═══ optimal cross-venue arb RIGHT NOW ═══");
    let (size, profit, buy_orca) = optimal_arb(&orca, &ray, &base, 50_000.0 * 1e6);
    println!("  optimal borrow={:.1} USDC → net profit={:.4} USDC, dir={} (after {}bp fees, before tip)",
        size / 1e6, profit / 1e6, if buy_orca { "buy-Orca/sell-Ray" } else { "buy-Ray/sell-Orca" }, cfg.round_trip_fee_bps());
    if profit <= 0.0 {
        println!("  → no profitable arb at this instant (expected on SOL/USDC)");
    }
}
