//! End-to-end fire-tx proof for MULTI-POSITION + 2-HOP routing against the real
//! whale account (HajGURDp…, 98% of the 10h census' liquidation value).
//!
//! For EACH of the whale's wired-debt legs, build the FULL fire tx exactly like
//! the streaming executor (flashloan-wrapped liquidate + withdraw + direct/2-hop
//! swap + payback, 7-position obs list) and simulate it. Expected while healthy:
//! liquidate ix reverts 6068 (HealthyAccount) — wiring + size + swap composition
//! proven; anything at a different ix, or a compile/size failure, is a bug.
//!
//! Usage: HELIUS_RPC=<url> [ACCOUNT=<pk>] cargo run --release --bin whale_fire_probe

use arb_engine::liq_fire::{self, FireCandidate};
use arb_engine::liquidation::{self as liq, Bank, MarginfiAccount};
use solana_instruction::AccountMeta;
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const DEFAULT_ACCOUNT: &str = "HajGURDp5mWYPMX7AucEQxnccgqrnRA8S8iLFsWfCxyL";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";

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
fn fetch_raw(endpoint: &str, pk: &Pubkey) -> Option<(Vec<u8>, Pubkey)> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    let val = &v["result"]["value"];
    Some((b64(&val["data"])?, Pubkey::from_str(val["owner"].as_str()?).ok()?))
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").expect("HELIUS_RPC");
    let account = Pubkey::from_str(&std::env::var("ACCOUNT").unwrap_or_else(|_| DEFAULT_ACCOUNT.into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();

    let (raw, _) = fetch_raw(&endpoint, &account).expect("account fetch");
    let acct = MarginfiAccount::decode(&raw).expect("decode");
    let obs_banks = liq::active_bank_pks(&raw);
    println!("account {account}: {} funded balances, {} active-flag banks (obs)", acct.balances.len(), obs_banks.len());

    // banks + oracles + token programs + prices
    let mut banks: HashMap<Pubkey, Bank> = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    let mut prices: HashMap<Pubkey, f64> = HashMap::new();
    let mut mint_tp: HashMap<Pubkey, Pubkey> = HashMap::new();
    for bpk in &obs_banks {
        let (braw, _) = fetch_raw(&endpoint, bpk).expect("bank fetch");
        let bank = Bank::decode(&braw).expect("bank decode");
        oracle_of.insert(*bpk, bank.oracle_key);
        if let Some((oraw, _)) = fetch_raw(&endpoint, &bank.oracle_key) {
            if let Some(p) = liq::decode_oracle_price(&oraw) { prices.insert(*bpk, p); }
        }
        if let Some((_, owner)) = fetch_raw(&endpoint, &bank.mint) { mint_tp.insert(bank.mint, owner); }
        banks.insert(*bpk, bank);
    }

    let asset = acct.balances.iter().filter(|b| b.asset_shares > 0.0).max_by(|x, y| {
        let ux = banks.get(&x.bank_pk).and_then(|bk| prices.get(&x.bank_pk).map(|p| x.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
        let uy = banks.get(&y.bank_pk).and_then(|bk| prices.get(&y.bank_pk).map(|p| y.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * p)).unwrap_or(0.0);
        ux.partial_cmp(&uy).unwrap_or(std::cmp::Ordering::Equal)
    }).expect("no collateral");
    let abk = &banks[&asset.bank_pk];
    println!("dominant collateral: bank {} mint {}", asset.bank_pk, abk.mint);

    let obs: Vec<AccountMeta> = obs_banks.iter().flat_map(|b| vec![
        AccountMeta::new_readonly(*b, false),
        AccountMeta::new_readonly(oracle_of[b], false),
    ]).collect();

    // one probe per wired-debt leg
    for debt in acct.balances.iter().filter(|b| b.liability_shares > 0.0) {
        let Some(lbk) = banks.get(&debt.bank_pk) else { continue };
        let debt_usd = debt.liability_shares * lbk.liability_share_value / 10f64.powi(lbk.mint_decimals as i32) * prices.get(&debt.bank_pk).copied().unwrap_or(0.0);
        let route = if abk.mint == lbk.mint { "same-mint".into() }
            else if liq_fire::direct_dex_pool(&abk.mint, &lbk.mint).is_some() { "direct".into() }
            else if let Some((_, mid, _)) = liq_fire::two_hop_route(&abk.mint, &lbk.mint) { format!("2-hop via {}", &mid.to_string()[..6]) }
            else { "NO ROUTE".into() };
        // seize a small rung (~$50 of collateral) — proving composition, not firing
        let px = prices.get(&asset.bank_pk).copied().unwrap_or(0.0);
        if px <= 0.0 { println!("  leg {} — no collateral price, skip", lbk.mint); continue; }
        let seize = (50.0 / px * 10f64.powi(abk.mint_decimals as i32)) as u64;
        let cand = FireCandidate {
            liquidatee: account,
            asset_bank: asset.bank_pk, asset_mint: abk.mint,
            asset_token_program: mint_tp.get(&abk.mint).copied().unwrap_or(Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap()),
            asset_amount: seize,
            liab_bank: debt.bank_pk, debt_mint: lbk.mint,
            debt_token_program: mint_tp.get(&lbk.mint).copied().unwrap_or(Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap()),
            asset_oracle: oracle_of[&asset.bank_pk], liab_oracle: oracle_of[&debt.bank_pk],
            liquidatee_obs: obs.clone(),
        };
        match liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority, None, 0, 100_000, 50, 20, solana_hash::Hash::default()) {
            Err(e) => println!("  leg {} (${:.0}, {route}) → BUILD FAIL: {e}", lbk.mint, debt_usd),
            Ok(f) => {
                use base64::Engine;
                let tx_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&f.tx).unwrap());
                let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
                    "params":[tx_b64, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
                let err = sim.as_ref().map(|v| v["result"]["value"]["err"].clone()).unwrap_or(serde_json::Value::Null);
                let verdict = if err.is_null() { "✅ FIREABLE NOW".into() }
                    else if let Some(ie) = err.get("InstructionError") {
                        let idx = ie.get(0).and_then(|i| i.as_u64());
                        let code = ie.get(1).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
                        // ix layout: cu, cu_price, ata, ata, [mid-ata], start_fl, LIQUIDATE, ...
                        match code {
                            Some(6068) => format!("✅ WIRING+ROUTE OK — HealthyAccount(6068) at ix {idx:?}"),
                            Some(c) => format!("⚠ Custom({c}) at ix {idx:?}"),
                            None => format!("⚠ {ie}"),
                        }
                    } else { format!("⚠ {err}") };
                println!("  leg {} (${:.0}, {route}) → {} bytes | {verdict}", lbk.mint, debt_usd, f.tx_bytes);
            }
        }
    }
}
