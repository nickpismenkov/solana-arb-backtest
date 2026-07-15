//! Wiring proof for MULTI-POSITION liquidation.
//!
//! The live fire path skips any account with >1 collateral or >1 debt
//! (liq_executor is_v1_fireable / try_arm's `assets.len()!=1 || liabs.len()!=1`).
//! The census showed that's where ~99% of at-risk collateral sits ($2.6M / $941k /
//! $791k positions). marginfi's `lending_account_liquidate` is single-leg (one
//! asset_bank, one liab_bank) but carries the FULL balance list in the
//! observation accounts — so liquidating ONE leg of a multi-position account is
//! supported by the program. This probe proves the single-leg tx COMPOSES against
//! a real multi-position account: build [start_fl, liquidate(one leg), end_fl] and
//! simulate. Outcome classification:
//!   err=null            → the leg is fireable right now (real opportunity)
//!   HealthyAccount 6068 → wiring OK; account healthy at this leg/size (expected calm)
//!   other Custom code   → an account-specific gate (stale oracle, etc.), still wiring-OK
//!   error at a DIFFERENT ix index → a WIRING BUG (what this probe exists to catch)
//!
//! Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] [TOPN=5] cargo run --release --bin mfi_multipos_probe

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
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const SOL_MINT: &str = "So11111111111111111111111111111111111111112";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
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
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"jsonParsed"}]}))?;
    Pubkey::from_str(v["result"]["value"]["owner"].as_str()?).ok()
}
fn is_debt_mint(m: &Pubkey) -> bool {
    let s = m.to_string();
    s == USDC_MINT || s == USDT_MINT || s == SOL_MINT
}

/// Build [start_fl, liquidate(asset_bank, liab_bank, amount), end_fl] as base64.
#[allow(clippy::too_many_arguments)]
fn gate_tx_b64(authority: &Pubkey, liquidator_ma: &Pubkey, tp: &Pubkey, liquidatee: &Pubkey,
    acct: &MarginfiAccount, asset_bank: Pubkey, liab_bank: Pubkey, asset_amount: u64,
    oracle_of: &HashMap<Pubkey, Pubkey>) -> Option<String> {
    use solana_message::{v0, VersionedMessage};
    let mut obs = Vec::new();
    for b in &acct.balances {
        obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        obs.push(AccountMeta::new_readonly(*oracle_of.get(&b.bank_pk)?, false));
    }
    let start = marginfi::start_flashloan(liquidator_ma, authority, 2);
    let liq_ix = marginfi::lending_account_liquidate(&asset_bank, &liab_bank, liquidator_ma, authority,
        liquidatee, tp, asset_amount, oracle_of.get(&asset_bank)?, oracle_of.get(&liab_bank)?, &obs);
    let end_obs = vec![
        AccountMeta::new_readonly(asset_bank, false), AccountMeta::new_readonly(*oracle_of.get(&asset_bank)?, false),
        AccountMeta::new_readonly(liab_bank, false), AccountMeta::new_readonly(*oracle_of.get(&liab_bank)?, false),
    ];
    let end = marginfi::end_flashloan(liquidator_ma, authority, &end_obs);
    let msg = v0::Message::try_compile(authority, &[start, liq_ix, end], &[], solana_hash::Hash::default()).ok()?;
    let tx = solana_transaction::versioned::VersionedTransaction {
        signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    use base64::Engine;
    Some(base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).ok()?))
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let topn: usize = std::env::var("TOPN").ok().and_then(|s| s.parse().ok()).unwrap_or(5);

    eprintln!("[mp] scanning marginfi group …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})).expect("scan");
    let accts: Vec<(Pubkey, MarginfiAccount)> = resp["result"].as_array().cloned().unwrap_or_default().iter()
        .filter_map(|e| Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?)))
        .filter(|(_, a): &(Pubkey, MarginfiAccount)| {
            let na = a.balances.iter().filter(|b| b.asset_shares > 0.0).count();
            let nl = a.balances.iter().filter(|b| b.liability_shares > 0.0).count();
            (na + nl) > 2 // multi-position: more than one collateral OR more than one debt
        }).collect();

    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw { if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0);
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut prices: PriceMap = HashMap::new();
    for (pk, raw) in &oracle_raw {
        if let Some(usd) = liq::decode_oracle_price_fresh(raw, slot, liq::DEFAULT_MAX_SB_STALE_SLOTS) {
            for (bk, oc) in &oracle_of { if oc == pk { prices.insert(*bk, usd); } }
        }
    }

    // Rank multi-position accounts by collateral USD (fresh-priced, complete health).
    let mut ranked: Vec<(Pubkey, &MarginfiAccount, f64, f64)> = Vec::new();
    for (pk, a) in &accts {
        let h = liq::maintenance_health(a, &banks, &prices);
        if h.missing > 0 || h.health.weighted_assets <= 0.0 { continue; }
        let coll: f64 = a.balances.iter().filter(|b| b.asset_shares > 0.0).filter_map(|b| {
            let bk = banks.get(&b.bank_pk)?; let px = prices.get(&b.bank_pk)?;
            Some(b.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * px)
        }).sum();
        ranked.push((*pk, a, coll, h.health.ratio()));
    }
    ranked.sort_by(|a, b| b.2.partial_cmp(&a.2).unwrap_or(std::cmp::Ordering::Equal));
    eprintln!("[mp] {} multi-position accounts (complete health); probing top {}\n", ranked.len(), topn);

    for (pk, a, coll, ratio) in ranked.iter().take(topn) {
        // Leg picker: choose the (collateral, debt) pair maximizing seized-collateral
        // USD, restricted to a wired debt mint (USDC/USDT/wSOL).
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        let debt_leg = liabs.iter().filter(|b| banks.get(&b.bank_pk).map(|bk| is_debt_mint(&bk.mint)).unwrap_or(false))
            .max_by(|x, y| {
                let vx = banks.get(&x.bank_pk).and_then(|bk| prices.get(&x.bank_pk).map(|p| x.liability_shares * bk.liability_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
                let vy = banks.get(&y.bank_pk).and_then(|bk| prices.get(&y.bank_pk).map(|p| y.liability_shares * bk.liability_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
                vx.partial_cmp(&vy).unwrap_or(std::cmp::Ordering::Equal)
            });
        let coll_leg = assets.iter().max_by(|x, y| {
            let vx = banks.get(&x.bank_pk).and_then(|bk| prices.get(&x.bank_pk).map(|p| x.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
            let vy = banks.get(&y.bank_pk).and_then(|bk| prices.get(&y.bank_pk).map(|p| y.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
            vx.partial_cmp(&vy).unwrap_or(std::cmp::Ordering::Equal)
        });
        let (Some(debt_leg), Some(coll_leg)) = (debt_leg, coll_leg) else {
            println!("  {} coll≈${:.0} ratio {:.3}  [SKIP: no wired-debt leg]", &pk.to_string()[..8], coll, ratio);
            continue;
        };
        let asset_bank = coll_leg.bank_pk; let liab_bank = debt_leg.bank_pk;
        let abk = &banks[&asset_bank];
        let native = coll_leg.asset_shares * abk.asset_share_value;
        let seize = (native * 0.02) as u64; // 2% rung — just proving composition
        let tp = mint_owner(&endpoint, &abk.mint).unwrap_or(Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap());
        let na = assets.len(); let nl = liabs.len();
        let Some(gate) = gate_tx_b64(&authority, &liquidator_ma, &tp, pk, a, asset_bank, liab_bank, seize, &oracle_of) else {
            println!("  {} [{}c/{}d] coll≈${:.0}  [tx build failed]", &pk.to_string()[..8], na, nl, coll); continue;
        };
        let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[gate, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
        let res = sim.as_ref().map(|v| &v["result"]["value"]);
        let err = res.map(|r| &r["err"]);
        if std::env::var("LOGS").ok().as_deref() == Some("1") {
            eprintln!("  --- {} RAW sim response ---", &pk.to_string()[..8]);
            eprintln!("{}", serde_json::to_string_pretty(sim.as_ref().unwrap_or(&serde_json::Value::Null)).unwrap_or_default());
        }
        let (idx, code) = err.and_then(|e| e.get("InstructionError"))
            .map(|ie| (ie.get(0).and_then(|i| i.as_u64()), ie.get(1).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64())))
            .unwrap_or((None, None));
        // An RPC-level error means the sim never ran (bad params/tx) — must never
        // be read as "no instruction error → fireable".
        if let Some(e) = sim.as_ref().and_then(|v| v.get("error")).filter(|e| !e.is_null()) {
            println!("  {} [{}c/{}d] coll≈${:.0}  →  ⚠ RPC error, sim did not run: {}",
                &pk.to_string()[..8], assets.len(), liabs.len(), coll, e["message"].as_str().unwrap_or(""));
            continue;
        }
        let verdict = match (err.map(|e| e.is_null()).unwrap_or(false), idx, code) {
            (true, _, _) => "✅ err=null — FIREABLE NOW (real multi-position opportunity)".into(),
            (false, Some(1), Some(6068)) => "✅ WIRING OK — liquidate ix ran, HealthyAccount(6068) at this leg/size".into(),
            (false, Some(1), Some(c)) => format!("✅ WIRING OK — liquidate ix ran, reverted in-ix Custom({c})"),
            (false, Some(i), c) => format!("⚠ error at ix {i} (not the liquidate ix) code={c:?} — INVESTIGATE"),
            _ => format!("? unclassified: {:?}", err),
        };
        println!("  {} [{}c/{}d] coll≈${:.0} ratio {:.3}  seize2%={}  →  {}",
            &pk.to_string()[..8], na, nl, coll, ratio, seize, verdict);
    }
    eprintln!("\n[mp] If every top account shows 'WIRING OK' (ix 1), single-leg liquidation composes on\n     multi-position accounts and the fix is purely a leg-PICKER in try_arm, not an N-leg tx rewrite.");
}
