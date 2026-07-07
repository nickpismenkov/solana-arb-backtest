//! Simulate the FULL atomic fire tx against a real marginfi candidate and
//! classify the result by instruction index — the wiring test for the fire
//! path. With 0 genuinely liquidatable accounts (current market), the expected
//! outcome is a revert AT THE LIQUIDATE IX with HealthyAccount(6068): that
//! still proves ATA creates + start_flashloan + the liquidate account wiring
//! execute, the Jupiter swap composes, and the tx compiles under 1232 bytes.
//! Any failure at a DIFFERENT index is a wiring bug. err=null (a real
//! liquidatable) verifies the whole path.
//!
//! Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] [MIN_COLLATERAL_USD=50]
//!        cargo run --release --bin liq_fire_probe

use arb_engine::liq_fire::{self, FireCandidate};
use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use solana_instruction::AccountMeta;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const LIQUIDATE_IX_INDEX: u64 = 5; // [cu, cu_price, ata, ata, start_fl, LIQUIDATE, …]

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
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]})) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(b) = acc.get("data").and_then(b64) { out.insert(chunk[i], b); }
        }
    }
    out
}
/// Owner program of a mint account (classic SPL vs Token-2022).
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"base64"}]}))?;
    v["result"]["value"]["owner"].as_str()?.parse().ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(50.0);
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();

    // Scan → banks → prices (same pipeline as liq_executor).
    eprintln!("[fire] scanning marginfi group …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let accts: Vec<(Pubkey, MarginfiAccount)> = entries.iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();

    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw { if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let mut prices: PriceMap = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &oracle_pks) {
        if let Some((_f, usd, _t)) = liq::decode_price_update_v2(raw) {
            for (bk, oc) in &oracle_of { if oc == pk { prices.insert(*bk, usd); } }
        }
    }

    // Best base-weight candidate with 1 collateral + 1 USDC debt.
    let mut best: Option<(Pubkey, &MarginfiAccount, Pubkey, f64)> = None;
    for (pk, a) in &accts {
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 || !r.health.liquidatable() || r.health.weighted_assets < min_collateral { continue; }
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        if assets.len() != 1 || liabs.len() != 1 || liabs[0].bank_pk != usdc_bank { continue; }
        if best.as_ref().map_or(true, |b| r.health.weighted_assets > b.3) {
            best = Some((*pk, a, assets[0].bank_pk, r.health.weighted_assets));
        }
    }
    let Some((liquidatee, acct, asset_bank, collat)) = best else {
        eprintln!("[fire] no base-weight candidate with single collateral + USDC debt found — nothing to wire-test against");
        return;
    };
    let asset_bk = &banks[&asset_bank];
    let asset_tp = mint_owner(&endpoint, &asset_bk.mint).expect("mint owner");
    let asset_bal = acct.balances.iter().find(|b| b.bank_pk == asset_bank).unwrap();
    let native = asset_bal.asset_shares * asset_bk.asset_share_value;
    let asset_amount = (native * 0.02) as u64;
    eprintln!("[fire] candidate {}  collateral≈${:.0}  asset mint {} (tp {})  seize {} native (2%)",
        &liquidatee.to_string()[..8], collat, asset_bk.mint, &asset_tp.to_string()[..8], asset_amount);

    let mut liquidatee_obs: Vec<AccountMeta> = Vec::new();
    for b in &acct.balances {
        liquidatee_obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        liquidatee_obs.push(AccountMeta::new_readonly(oracle_of[&b.bank_pk], false));
    }
    let cand = FireCandidate {
        liquidatee,
        asset_bank,
        asset_mint: asset_bk.mint,
        asset_token_program: asset_tp,
        asset_amount,
        liab_bank: usdc_bank,
        asset_oracle: oracle_of[&asset_bank],
        liab_oracle: oracle_of[&usdc_bank],
        liquidatee_obs,
    };

    eprintln!("[fire] building fire tx (Jupiter quote + ALTs) …");
    let fire = liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority,
        None, 0, 100_000, 100, 20, solana_hash::Hash::default()).expect("build fire tx");
    eprintln!("[fire] tx {} bytes (limit 1232)  quoted_usdc_out={}", fire.tx_bytes, fire.quoted_usdc_out);

    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))
        .expect("simulate");
    if sim.get("result").map(|r| r.get("value").is_some()) != Some(true) {
        println!("✗ RPC rejected the simulation (no result.value): {sim}");
        return;
    }
    let res = &sim["result"]["value"];
    println!("\n──── fire-path simulation ────");
    println!("err: {}", res["err"]);
    println!("unitsConsumed: {}", res["unitsConsumed"]);
    let ix_idx = res["err"].get("InstructionError").and_then(|e| e.get(0)).and_then(|i| i.as_u64());
    let code = res["err"].get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
    match (res["err"].is_null(), ix_idx, code) {
        (true, _, _) => println!("★★ FULL FIRE PATH VERIFIED — genuinely liquidatable candidate, whole tx executes"),
        (_, Some(LIQUIDATE_IX_INDEX), Some(6068)) => println!(
            "★ WIRING OK — reverted at the liquidate ix with HealthyAccount (emode phantom, expected \
             with 0 real liquidatable): ATAs + start_flashloan + liquidate accounts + swap compose verified"),
        (_, Some(i), c) => {
            println!("✗ UNEXPECTED failure at ix {} (custom {:?}) — wiring bug, logs:", i, c);
            for l in res["logMessages"].as_array().into_iter().flatten() { println!("  {}", l.as_str().unwrap_or("")); }
        }
        _ => println!("? inconclusive: {:?}", res["err"]),
    }
}
