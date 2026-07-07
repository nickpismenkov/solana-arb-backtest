//! marginfi liquidation SIMULATION probe — assembles the flashloan-wrapped
//! liquidate against a REAL liquidatable account and simulates it on mainnet
//! (sigVerify=false, replaceRecentBlockhash). Proves the instruction wiring
//! executes: we want to see the LendingAccountLiquidate handler run (state
//! change or a meaningful marginfi error), NOT a deserialize/account error.
//!
//! Picks the top liquidatable borrower with exactly one collateral + one debt
//! bank (simplest case). Tx = [start_flashloan, liquidate(2% of collateral),
//! end_flashloan]; end_flashloan re-checks health over both liquidator balances.
//!
//! Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=<acct>] [AUTHORITY=<pk>] cargo run --release --bin liq_marginfi_sim

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use solana_instruction::AccountMeta;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
// Liquidator marginfi account created earlier (authority = arb wallet).
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";

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

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let tp = Pubkey::from_str(TOKEN_PROGRAM).unwrap();

    // 1) Scan group → borrowers.
    eprintln!("[sim] scanning marginfi group …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    // keep account pubkey alongside the decoded account.
    let accts: Vec<(Pubkey, MarginfiAccount)> = entries.iter().filter_map(|e| {
        let pk = e["pubkey"].as_str()?.parse::<Pubkey>().ok()?;
        let a = MarginfiAccount::decode(&b64(&e["account"]["data"])?)?;
        Some((pk, a))
    }).filter(|(_, a)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();
    eprintln!("[sim] {} borrowers", accts.len());

    // 2) Banks + oracle prices.
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk))
        .collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw {
        if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); }
    }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut oprice: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, raw) in &oracle_raw { if let Some(usd) = liq::decode_oracle_price(raw) { oprice.insert(*pk, usd); } }
    let mut prices: PriceMap = HashMap::new();
    for (bk, oc) in &oracle_of { if let Some(&p) = oprice.get(oc) { prices.insert(*bk, p); } }

    // 3) Pick top liquidatable with exactly 1 collateral + 1 debt bank, both priced.
    let mut best: Option<(Pubkey, &MarginfiAccount, Pubkey, Pubkey, f64)> = None; // (acct, a, asset_bank, liab_bank, collateral_usd)
    for (pk, a) in &accts {
        let r = liq::maintenance_health(a, &banks, &prices);
        if r.missing > 0 || !r.health.liquidatable() { continue; }
        let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
        let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
        if assets.len() != 1 || liabs.len() != 1 { continue; }
        if r.health.weighted_assets < 50.0 { continue; }
        if best.as_ref().map_or(true, |b| r.health.weighted_assets > b.4) {
            best = Some((*pk, a, assets[0].bank_pk, liabs[0].bank_pk, r.health.weighted_assets));
        }
    }
    let Some((liquidatee, acct, asset_bank, liab_bank, collat)) = best else {
        eprintln!("[sim] no single-collateral/single-debt liquidatable account found"); return;
    };
    let asset_bk = &banks[&asset_bank];
    let asset_oracle = oracle_of[&asset_bank];
    let liab_oracle = oracle_of[&liab_bank];
    // asset_amount = 2% of the liquidatee's collateral native units.
    let asset_bal = acct.balances.iter().find(|b| b.bank_pk == asset_bank).unwrap();
    let native = asset_bal.asset_shares * asset_bk.asset_share_value;
    let asset_amount = (native * 0.02) as u64;
    // Diagnostic: reconcile my weights vs marginfi's on-chain calc.
    let px = prices.get(&asset_bank).copied().unwrap_or(0.0);
    let dec = 10f64.powi(asset_bk.mint_decimals as i32);
    let raw_val = native / dec * px;
    eprintln!("[sim] asset_bank {} decimals={} price=${:.4}", asset_bank, asset_bk.mint_decimals, px);
    eprintln!("[sim] asset_weight_init={:.4} asset_weight_maint={:.4}", asset_bk.asset_weight_init, asset_bk.asset_weight_maint);
    eprintln!("[sim] raw collateral value=${:.0}  × init={:.0}  × maint={:.0}  (marginfi said assets=$39558)",
        raw_val, raw_val * asset_bk.asset_weight_init, raw_val * asset_bk.asset_weight_maint);
    eprintln!("[sim] liquidatee {} collateral=${:.0}", &liquidatee.to_string()[..8], collat);
    eprintln!("[sim] asset_bank {}… liab_bank {}… asset_amount={} (2% of {:.0} native)",
        &asset_bank.to_string()[..8], &liab_bank.to_string()[..8], asset_amount, native);

    // 4) Build flashloan-wrapped [start_fl, liquidate, end_fl].
    // liquidatee obs: for each active balance [bank, oracle] in slot order.
    let mut liquidatee_obs: Vec<AccountMeta> = Vec::new();
    for b in &acct.balances {
        liquidatee_obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        liquidatee_obs.push(AccountMeta::new_readonly(oracle_of[&b.bank_pk], false));
    }
    let end_index = 2u64; // ixs: 0 start_fl, 1 liquidate, 2 end_fl
    let start = marginfi::start_flashloan(&liquidator_ma, &authority, end_index);
    let liq_ix = marginfi::lending_account_liquidate(
        &asset_bank, &liab_bank, &liquidator_ma, &authority, &liquidatee, &tp,
        asset_amount, &asset_oracle, &liab_oracle, &liquidatee_obs);
    // end_flashloan obs = liquidator's post-liquidation balances: seized asset + new liab.
    let end_obs = vec![
        AccountMeta::new_readonly(asset_bank, false), AccountMeta::new_readonly(asset_oracle, false),
        AccountMeta::new_readonly(liab_bank, false), AccountMeta::new_readonly(liab_oracle, false),
    ];
    let end = marginfi::end_flashloan(&liquidator_ma, &authority, &end_obs);

    // 5) Assemble a v0 tx + simulate (sigVerify=false, replaceRecentBlockhash).
    use solana_message::{v0, VersionedMessage};
    use solana_transaction::versioned::VersionedTransaction;
    let bh = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
        .and_then(|v| v["result"]["value"]["blockhash"].as_str().map(String::from)).unwrap();
    let bh = solana_hash::Hash::from_str(&bh).unwrap();
    let msg = v0::Message::try_compile(&authority, &[start, liq_ix, end], &[], bh).unwrap();
    let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap()) };

    eprintln!("[sim] simulating …");
    let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
    let Some(sim) = sim else { eprintln!("[sim] no response"); return };
    let res = &sim["result"]["value"];
    println!("\n──── simulation result ────");
    println!("err: {}", res["err"]);
    if let Some(logs) = res["logMessages"].as_array() {
        for l in logs { println!("  {}", l.as_str().unwrap_or("")); }
    } else {
        println!("  (no logs — {:?})", sim);
    }
}
