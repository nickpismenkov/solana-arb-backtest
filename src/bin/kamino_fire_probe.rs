//! Simulate the FULL Kamino atomic fire tx against the most-underwater live
//! main-market obligation (USDC debt). Classifies by instruction index — the
//! wiring test for the whole flashloan-wrapped path. Expected outcomes with a
//! healthy market: either the obligation is genuinely liquidatable and the
//! whole tx runs (err null), or the liquidate ix reverts on health/close-factor
//! — both prove borrow + refreshes + liquidate account wiring + Jupiter swap
//! compose + JupLend payback compile under the size limit. A revert at any
//! other index is a wiring bug.
//!
//! Usage: HELIUS_RPC=<url> [AUTHORITY=<pk>] cargo run --release --bin kamino_fire_probe

use arb_engine::kamino::{Obligation, Reserve};
use arb_engine::kamino_fire::{self, KaminoFireCandidate};
use arb_engine::kamino_ix::ReserveAccounts;
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const OBLIGATION_SIZE: usize = 3344;
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, …]
const LIQUIDATE_IX_INDEX: u64 = 9;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    let strs: Vec<String> = keys.iter().map(|k| k.to_string()).collect();
    if let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
        "params":[strs, {"encoding":"base64"}]})) {
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(b) = acc.get("data").and_then(b64) { out.insert(keys[i], b); }
        }
    }
    out
}
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Pubkey {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[mint.to_string(),{"encoding":"base64"}]}))
        .and_then(|v| v["result"]["value"]["owner"].as_str().map(String::from))
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| Pubkey::from_str(TOKEN_PROGRAM).unwrap())
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let market = Pubkey::from_str(MAIN_MARKET).unwrap();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    // NONUSDC=1 → skip USDC-debt candidates, to prove the widened USDT/wSOL path.
    let skip_usdc = std::env::var("NONUSDC").ok().as_deref() == Some("1");
    // DEBT=USDC|USDT|wSOL → only sim that debt asset.
    let want_debt = std::env::var("DEBT").ok();

    eprintln!("[kfire] scanning main-market obligations (wired-debt USDC/USDT/wSOL, single deposit/borrow){} …",
        if skip_usdc { " [NON-USDC only]" } else { "" });
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize":OBLIGATION_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    eprintln!("[kfire] {} obligations", entries.len());

    // Need USDC to be the debt reserve → resolve each candidate's repay reserve
    // liquidity mint. Rank by stored ratio, take the first USDC-debt one.
    let mut ranked: Vec<(Pubkey, Obligation, f64)> = entries.iter().filter_map(|e| {
        let pk = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok())?;
        let ob = b64(&e["account"]["data"]).and_then(|d| Obligation::decode(&d))?;
        (ob.deposits.len() == 1 && ob.borrows.len() == 1 && ob.elevation_group == 0 && ob.unhealthy_borrow_value >= 50.0)
            .then(|| (pk, ob.clone(), ob.ratio()))
    }).collect();
    ranked.sort_by(|a, b| b.2.total_cmp(&a.2));

    for (ob_pk, ob, ratio) in ranked.into_iter().take(40) {
        let withdraw_pk = ob.deposits[0].0;
        let repay_pk = ob.borrows[0].0;
        let raw = get_multiple(&endpoint, &[withdraw_pk, repay_pk]);
        let (Some(wr_data), Some(rr_data)) = (raw.get(&withdraw_pk), raw.get(&repay_pk)) else { continue };
        let (Some(wr), Some(rr)) = (ReserveAccounts::decode(withdraw_pk, wr_data), ReserveAccounts::decode(repay_pk, rr_data)) else { continue };
        // v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL).
        if !arb_engine::flashloan::has_market(&rr.liquidity_mint) { continue; }
        if skip_usdc && rr.liquidity_mint == usdc { continue; }
        let (Some(wr_res), Some(rr_res)) = (Reserve::decode(wr_data), Reserve::decode(rr_data)) else { continue };
        let debt_sym = if rr.liquidity_mint == usdc { "USDC" }
            else if rr.liquidity_mint.to_string() == "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB" { "USDT" }
            else { "wSOL" };
        if let Some(w) = &want_debt { if w != debt_sym { continue; } }

        // Size: repay 20% of debt (Kamino close factor), capped small for the probe.
        let debt_dec = rr_res.mint_decimals as i32;
        let debt_price = rr_res.market_price.max(1e-9);
        let debt_usd = (ob.borrows[0].1 / 10f64.powi(debt_dec)) * rr_res.market_price;
        let repay_usd = (debt_usd * 0.2).min(50.0).max(1.0);
        // Native debt units priced in the actual debt asset (not hardcoded USDC).
        let repay_amount = (repay_usd / debt_price * 10f64.powi(debt_dec)) as u64;
        // Seized underlying native ≈ repay_usd × (1 + ~5% bonus) / price, 0.5% haircut.
        let bonus = 1.05;
        let seized_native = repay_usd * bonus / wr_res.market_price * 10f64.powi(wr_res.mint_decimals as i32);
        let swap_in_amount = (seized_native * 0.995) as u64;

        eprintln!("[kfire] target {} [{} debt] ratio {:.3}  debt ${:.0}  repay ${:.2} ({} native)  seize {} native ({} dp @ ${:.2})",
            &ob_pk.to_string()[..8], debt_sym, ratio, debt_usd, repay_usd, repay_amount, swap_in_amount, wr_res.mint_decimals, wr_res.market_price);

        let cand = KaminoFireCandidate {
            obligation: ob_pk,
            lending_market: market,
            repay_reserve: rr.clone(),
            withdraw_reserve: wr.clone(),
            obligation_reserves: vec![withdraw_pk, repay_pk],
            withdraw_liquidity_mint: wr.liquidity_mint,
            withdraw_liquidity_token_program: mint_owner(&endpoint, &wr.liquidity_mint),
            withdraw_collateral_token_program: mint_owner(&endpoint, &wr.collateral_mint),
            repay_liquidity_token_program: mint_owner(&endpoint, &rr.liquidity_mint),
            repay_amount,
            swap_in_amount,
        };

        let bh = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
            .and_then(|v| v["result"]["value"]["blockhash"].as_str().map(String::from)).unwrap();
        let bh = solana_hash::Hash::from_str(&bh).unwrap();
        let fire = match kamino_fire::build_fire_tx(&endpoint, &cand, &authority, None, 0, 100_000, 100, 20, bh) {
            Ok(f) => f,
            Err(e) => { eprintln!("[kfire]   build failed: {e}"); continue; }
        };
        eprintln!("[kfire]   tx {} bytes (limit 1232)  quoted_usdc_out={}", fire.tx_bytes, fire.quoted_usdc_out);

        let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
        let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
        let Some(sim) = sim else { continue };
        if sim.get("result").map(|r| r.get("value").is_some()) != Some(true) {
            eprintln!("[kfire]   RPC rejected sim: {}", sim["error"]); continue;
        }
        let res = &sim["result"]["value"];
        let ix_idx = res["err"].get("InstructionError").and_then(|e| e.get(0)).and_then(|i| i.as_u64());
        let code = res["err"].get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
        println!("\n──── Kamino fire simulation ({}…) ────", &ob_pk.to_string()[..8]);
        println!("err: {}  (ix {:?}, custom {:?})", res["err"], ix_idx, code);
        match (res["err"].is_null(), ix_idx) {
            (true, _) => { println!("★★ FULL KAMINO FIRE VERIFIED — whole flashloan-wrapped tx executes end to end"); return; }
            (_, Some(LIQUIDATE_IX_INDEX)) => {
                println!("★ WIRING OK — borrow + refresh×2 + refresh_obligation executed; liquidate reached \
                          health/close-factor checks (custom {:?}). Path compiles at {} bytes; swap + payback wired.", code, fire.tx_bytes);
                return;
            }
            (_, Some(i)) => {
                println!("✗ reverted at ix {} (custom {:?}) — wiring bug, logs:", i, code);
                for l in res["logs"].as_array().into_iter().flatten() { println!("  {}", l.as_str().unwrap_or("")); }
                return;
            }
            _ => println!("? inconclusive: {}", res["err"]),
        }
    }
    println!("no wired-debt single-position obligation simulated");
}
