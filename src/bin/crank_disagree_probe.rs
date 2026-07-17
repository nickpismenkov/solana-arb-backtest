//! Crank-correctness diagnostic: for a SPECIFIC account the streaming executor
//! crank-fired, replicate the executor's exact judge (fresh_base + insert the
//! Hermes price for the dominant collateral bank), print OUR maintenance_health
//! verdict, then simulateBundle [setup, crank, liquidate] and print MARGINFI's
//! verdict at the same posted price. If our judge says liquidatable (ratio>=1.0)
//! but marginfi returns 6068 HealthyAccount, our health calc or price disagrees
//! with the chain at the identical price — the correctness bug. If the bundle
//! sims CLEAN, our fire was correct and we lost the Jito auction (a speed/tip
//! problem, not correctness).
//!
//! Usage: HELIUS_RPC=<url> ACCOUNT=<pk> [SEIZE=<native u64>] cargo run --release --bin crank_disagree_probe

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use arb_engine::pyth_accumulator as acc;
use arb_engine::pyth_crank;
use solana_instruction::AccountMeta;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

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
fn get_raw(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
}
fn tx_b64(tx: &VersionedTransaction) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx).unwrap())
}
fn hexs(b: &[u8]) -> String { b.iter().map(|x| format!("{x:02x}")).collect() }

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").expect("HELIUS_RPC");
    let hermes = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let account = Pubkey::from_str(&std::env::var("ACCOUNT").expect("ACCOUNT")).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let default_stale = liq::DEFAULT_MAX_SB_STALE_SLOTS;
    let tp = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    let raw = get_raw(&endpoint, &account).expect("account");
    let a = MarginfiAccount::decode(&raw).expect("decode");
    let obs_banks = liq::active_bank_pks(&raw);
    let slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0);

    // banks + oracles
    let bank_pks: Vec<Pubkey> = obs_banks.clone();
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    let mut oracle_raw: HashMap<Pubkey, Vec<u8>> = HashMap::new();
    for bpk in &bank_pks {
        if let Some(braw) = get_raw(&endpoint, bpk) {
            if let Some(bk) = Bank::decode(&braw) {
                if let Some(oraw) = get_raw(&endpoint, &bk.oracle_key) { oracle_raw.insert(bk.oracle_key, oraw); }
                oracle_of.insert(*bpk, bk.oracle_key);
                banks.insert(*bpk, bk);
            }
        }
    }

    // fresh_base: decode_oracle_price_fresh per bank (exactly like the executor)
    let mut base: PriceMap = PriceMap::new();
    for (bk, oc) in &oracle_of {
        let max_age = banks.get(bk).map(|b| b.oracle_max_age).unwrap_or(0);
        let max_stale = liq::max_stale_slots_for(max_age, default_stale);
        if let Some(usd) = oracle_raw.get(oc).and_then(|r| liq::decode_oracle_price_fresh(r, slot, max_stale)) {
            base.insert(*bk, usd);
        }
    }

    // Dominant collateral bank = largest USD asset (fresh-priced), like the executor.
    let dom = a.balances.iter().filter(|b| b.asset_shares > 0.0).filter_map(|b| {
        let bk = banks.get(&b.bank_pk)?; let p = base.get(&b.bank_pk)?;
        Some((b.bank_pk, b.asset_shares * bk.asset_share_value / 10f64.powi(bk.mint_decimals as i32) * p))
    }).max_by(|x, y| x.1.partial_cmp(&y.1).unwrap_or(std::cmp::Ordering::Equal));
    let Some((asset_bank, _)) = dom else { println!("no fresh-priced collateral"); return };
    let abk = banks[&asset_bank].clone();

    // Feed id + crankable?
    let feed_id = oracle_raw.get(&oracle_of[&asset_bank])
        .and_then(|r| liq::decode_price_update_v2(r)).map(|(f, _, _)| f);
    let Some(feed_id) = feed_id else { println!("asset bank oracle not a Pyth PriceUpdateV2 (not crankable)"); return };
    let crankable = pyth_crank::sponsored_feed(0, &feed_id) == oracle_of[&asset_bank];

    // Fresh Hermes update for that feed → the price WE would post.
    let fid_hex = hexs(&feed_id);
    let update = acc::fetch_hermes(&hermes, &[&fid_hex]).expect("hermes");
    let mu = update.updates.iter().find(|u| u.feed_id() == Some(feed_id)).expect("feed in blob");
    let hermes_px = mu.price_usd().expect("price_usd");

    // OUR JUDGE: insert the Hermes price for the asset bank, health over fresh_base.
    let mut jprices = base.clone();
    jprices.insert(asset_bank, hermes_px);
    let hj = liq::maintenance_health(&a, &banks, &jprices);

    println!("account {account}");
    println!("  dominant collateral bank {}  mint {}  crankable={crankable}", &asset_bank.to_string()[..8], abk.mint);
    println!("  Hermes price we'd post: ${hermes_px:.8}");
    println!("  on-chain price now:     ${:?}", oracle_raw.get(&oracle_of[&asset_bank]).and_then(|r| liq::decode_oracle_price(r)));
    println!("  --- OUR JUDGE at Hermes price: ratio {:.5} (>=1.0 => we fire)  missing={}  wa=${:.2} wl=${:.2}",
        hj.health.ratio(), hj.missing, hj.health.weighted_assets, hj.health.weighted_liabilities);
    // Per-leg: which price each bank gets, and whether it's fresh
    println!("  --- per-leg prices (bank / mint / base-fresh? / judge price) ---");
    for b in &a.balances {
        if b.asset_shares == 0.0 && b.liability_shares == 0.0 { continue; }
        let bk = banks.get(&b.bank_pk);
        let mint = bk.map(|k| k.mint.to_string()).unwrap_or_default();
        let onchain = oracle_raw.get(&oracle_of[&b.bank_pk]).and_then(|r| liq::decode_oracle_price(r));
        let fresh = base.get(&b.bank_pk).copied();
        let judged = jprices.get(&b.bank_pk).copied();
        println!("     {}… {} a={:.0} l={:.0} onchain={:?} fresh={:?} judge={:?}",
            &b.bank_pk.to_string()[..8], &mint[..mint.len().min(6)], b.asset_shares, b.liability_shares, onchain, fresh, judged);
    }

    // Build the SAME bundle the executor sends: [setup, crank, (start_fl·liquidate·end_fl)].
    let seize: u64 = std::env::var("SEIZE").ok().and_then(|s| s.parse().ok())
        .unwrap_or(((a.balances.iter().find(|b| b.bank_pk == asset_bank).map(|b| b.asset_shares).unwrap_or(0.0) * abk.asset_share_value) * 0.1) as u64);
    let txs = pyth_crank::build_crank_txs(&authority, &update.vaa, std::slice::from_ref(mu), 0, 0, solana_hash::Hash::default()).expect("crank build");
    let (setup_b64, crank_b64) = txs.to_b64().unwrap();

    // Debt leg = USDC if present else first liability. Match the executor's obs (all active banks).
    let usdc_bank = a.balances.iter().find(|b| b.liability_shares > 0.0 && banks.get(&b.bank_pk).map(|k| k.mint.to_string() == USDC).unwrap_or(false))
        .map(|b| b.bank_pk)
        .or_else(|| a.balances.iter().find(|b| b.liability_shares > 0.0).map(|b| b.bank_pk))
        .expect("no liability");
    let mut obs = Vec::new();
    for bp in &obs_banks {
        obs.push(AccountMeta::new_readonly(*bp, false));
        obs.push(AccountMeta::new_readonly(oracle_of[bp], false));
    }
    let start = marginfi::start_flashloan(&liquidator_ma, &authority, 2);
    let liq_ix = marginfi::lending_account_liquidate(
        &asset_bank, &usdc_bank, &liquidator_ma, &authority, &account, &tp, seize,
        &oracle_of[&asset_bank], &oracle_of[&usdc_bank], &obs);
    let end_obs = vec![
        AccountMeta::new_readonly(asset_bank, false), AccountMeta::new_readonly(oracle_of[&asset_bank], false),
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
    if let Some(e) = v.get("error").filter(|e| !e.is_null()) { println!("  simulateBundle error: {e}"); return; }
    let val = &v["result"]["value"];
    let results = val["transactionResults"].as_array().cloned().unwrap_or_default();
    println!("  --- MARGINFI verdict (simulateBundle, seize={seize}) ---");
    for (i, r) in results.iter().enumerate() {
        let label = ["setup", "crank", "liquidate"][i.min(2)];
        println!("     tx[{i}] {label}: err={} cu={}", r["err"], r["unitsConsumed"]);
    }
    // liquidate logs (the ix errors reveal 6068 vs 6049 vs other)
    if let Some(logs) = results.get(2).and_then(|r| r["logs"].as_array()) {
        for l in logs.iter().filter_map(|l| l.as_str()).filter(|l| l.contains("Error") || l.contains("6068") || l.contains("6049") || l.contains("Liquidate")) {
            println!("     log: {l}");
        }
    }
    let gate_ok = results.get(2).map(|r| r["err"].is_null()).unwrap_or(false);
    println!("\n  VERDICT: our judge ratio {:.4} ({}), marginfi liquidate {}",
        hj.health.ratio(), if hj.health.ratio() >= 1.0 { "WE FIRE" } else { "we'd skip" },
        if gate_ok { "ACCEPTS (clean → we lost the AUCTION, not a revert)".to_string() }
        else { format!("REJECTS {} (judge disagrees with chain — CORRECTNESS)", results.get(2).map(|r| r["err"].to_string()).unwrap_or_default()) });
}
