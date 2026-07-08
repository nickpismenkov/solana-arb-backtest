//! Step-5 gate: simulate the FULL crank+liquidate bundle against REAL marginfi
//! accounts. Scans for the nearest-to-threshold borrowers whose asset bank has
//! a crankable (shard-0 sponsored) oracle, fetches a fresh Hermes update for
//! that feed, and simulateBundles:
//!
//!   [crank_setup, crank_fire, (start_fl · liquidate · end_fl)]
//!
//! Expected on a healthy market: crank txs SUCCEED (feed advances) and the
//! liquidate hits marginfi's HealthyAccount guard (custom 6068) — proving the
//! whole chain composes and the chain judged AT the cranked price. If an
//! account is genuinely underwater at the true price, the gate passes outright
//! (that's a live opportunity).
//!
//! Usage: HELIUS_RPC=<url> [TOP=3] [SEIZE_FRAC=0.1] cargo run --release --bin liq_crank_probe

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use arb_engine::pyth_accumulator as acc;
use arb_engine::pyth_crank;
use solana_instruction::AccountMeta;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const HEALTHY_ACCOUNT_ERR: u64 = 6068;

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

fn hexs(b: &[u8]) -> String { b.iter().map(|x| format!("{x:02x}")).collect() }

fn tx_b64(v: &VersionedTransaction) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(bincode::serialize(v).unwrap())
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let top: usize = std::env::var("TOP").ok().and_then(|s| s.parse().ok()).unwrap_or(3);
    let seize_frac: f64 = std::env::var("SEIZE_FRAC").ok().and_then(|s| s.parse().ok()).unwrap_or(0.1);
    let hermes = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();
    let tp = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    // Scan borrowers + banks (same shape as the executor's full_scan).
    eprintln!("[probe] scanning marginfi accounts…");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})).expect("scan");
    let accts: Vec<(Pubkey, MarginfiAccount)> = resp["result"].as_array().cloned().unwrap_or_default().iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &bank_pks) {
        if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); }
    }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut feed_of: HashMap<Pubkey, [u8; 32]> = HashMap::new();
    let mut crankable: HashSet<Pubkey> = HashSet::new();
    let mut prices = PriceMap::new();
    for (bank, oracle) in &oracle_of {
        let Some(raw) = oracle_raw.get(oracle) else { continue };
        if let Some(usd) = liq::decode_oracle_price(raw) { prices.insert(*bank, usd); }
        if let Some((fid, _, _)) = liq::decode_price_update_v2(raw) {
            feed_of.insert(*bank, fid);
            if pyth_crank::sponsored_feed(0, &fid) == *oracle { crankable.insert(*bank); }
        }
    }
    eprintln!("[probe] {} borrowers, {} banks, {} crankable", accts.len(), banks.len(), crankable.len());

    // Candidates: 1-asset/1-liab-USDC, crankable asset bank, ranked by ratio.
    let mut cands: Vec<(f64, Pubkey, MarginfiAccount, Pubkey)> = accts.iter().filter_map(|(pk, a)| {
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        if assets.len() != 1 || liabs.len() != 1 || liabs[0].bank_pk != usdc_bank { return None; }
        if !crankable.contains(&assets[0].bank_pk) { return None; }
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 || r.health.weighted_assets < 50.0 { return None; }
        Some((r.health.ratio(), *pk, a.clone(), assets[0].bank_pk))
    }).collect();
    cands.sort_by(|a, b| b.0.partial_cmp(&a.0).unwrap());
    cands.truncate(top);

    let mut chain_verified = 0usize;
    for (ratio, pk, a, asset_bank) in &cands {
        let feed_id = feed_of[asset_bank];
        let bank = &banks[asset_bank];
        println!("\n════ candidate {}  ratio {ratio:.4}  asset bank {}…  feed {}…",
            pk, &asset_bank.to_string()[..8], &hexs(&feed_id)[..16]);

        // Fresh Hermes update → crank txs.
        let fid_hex = hexs(&feed_id);
        let update = match acc::fetch_hermes(&hermes, &[&fid_hex]) {
            Ok(u) => u, Err(e) => { println!("  ✗ hermes: {e}"); continue }
        };
        let Some(mu) = update.updates.iter().find(|u| u.feed_id() == Some(feed_id)) else {
            println!("  ✗ feed missing from blob"); continue
        };
        let txs = match pyth_crank::build_crank_txs(&authority, &update.vaa, std::slice::from_ref(mu),
            0, 0, solana_hash::Hash::default()) {
            Ok(t) => t, Err(e) => { println!("  ✗ crank build: {e}"); continue }
        };
        let (setup_b64, crank_b64) = txs.to_b64().unwrap();

        // Gate tx at SEIZE_FRAC of the collateral.
        let native_total = a.balances.iter().find(|b| b.asset_shares > 0.0).map(|b| b.asset_shares * bank.asset_share_value).unwrap_or(0.0);
        let amount = (native_total * seize_frac) as u64;
        let mut obs = Vec::new();
        for b in &a.balances {
            obs.push(AccountMeta::new_readonly(b.bank_pk, false));
            obs.push(AccountMeta::new_readonly(oracle_of[&b.bank_pk], false));
        }
        let start = marginfi::start_flashloan(&liquidator_ma, &authority, 2);
        let liq_ix = marginfi::lending_account_liquidate(
            asset_bank, &usdc_bank, &liquidator_ma, &authority, pk, &tp, amount,
            &oracle_of[asset_bank], &oracle_of[&usdc_bank], &obs);
        let end_obs = vec![
            AccountMeta::new_readonly(*asset_bank, false), AccountMeta::new_readonly(oracle_of[asset_bank], false),
            AccountMeta::new_readonly(usdc_bank, false), AccountMeta::new_readonly(oracle_of[&usdc_bank], false),
        ];
        let end = marginfi::end_flashloan(&liquidator_ma, &authority, &end_obs);
        let msg = v0::Message::try_compile(&authority, &[start, liq_ix, end], &[], solana_hash::Hash::default()).unwrap();
        let gate = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };

        let v = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateBundle",
            "params":[{"encodedTransactions":[setup_b64, crank_b64, tx_b64(&gate)]}, {
                "skipSigVerify": true, "replaceRecentBlockhash": true,
                "preExecutionAccountsConfigs": [null, null, null],
                "postExecutionAccountsConfigs": [null, null, null]
            }]})).expect("simulateBundle");
        if let Some(e) = v.get("error").filter(|e| !e.is_null()) {
            println!("  ✗ simulateBundle error: {e}"); continue
        }
        let val = &v["result"]["value"];
        let results = val["transactionResults"].as_array().cloned().unwrap_or_default();
        let ok = results.iter().filter(|r| r["err"].is_null()).count();
        println!("  bundle: {} of 3 txs succeeded  summary={}", ok,
            if val["summary"] == "succeeded" { "succeeded".into() } else { val["summary"].to_string() });
        for (i, r) in results.iter().enumerate() {
            println!("    tx[{i}] err={} cu={}", r["err"], r["unitsConsumed"]);
        }
        // Crank landed iff the first two txs succeeded; the gate's verdict is
        // then the chain's judgment AT the cranked price.
        let crank_ok = results.len() >= 2 && results[..2].iter().all(|r| r["err"].is_null());
        let gate_code = results.get(2)
            .and_then(|r| r["err"].get("InstructionError")).and_then(|e| e.get(1))
            .and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
        if crank_ok {
            if ok == 3 {
                println!("  ★★ LIVE OPPORTUNITY — liquidate ACCEPTED at the cranked price (would seize {amount})");
                chain_verified += 1;
            } else if gate_code == Some(HEALTHY_ACCOUNT_ERR) {
                println!("  ★ CHAIN-VERIFIED — crank landed, marginfi judged at the fresh price: HealthyAccount (6068), account not (yet) underwater");
                chain_verified += 1;
            } else {
                println!("  ⚠ crank landed but liquidate failed with custom {gate_code:?} (emode/size guard?)");
            }
        } else {
            println!("  ✗ crank txs failed in bundle — inspect logs above");
        }
    }
    println!("\n{} of {} candidates chain-verified through the crank bundle", chain_verified, cands.len());
}
