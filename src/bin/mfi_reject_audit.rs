//! Why do accounts our engine flags get rejected? Audit the REAL revert codes.
//!
//! liq_executor logs "chain says healthy at the actionable price" whenever the
//! size-ladder sim returns Some(false) — but simulate_gate maps EVERY custom
//! error to Some(false), not just HealthyAccount(6068). So that one log line
//! actually hides: 6068 (genuinely healthy — if OUR maintenance_health says
//! liquidatable at the SAME on-chain price, that's a health-MATH bug), 6049
//! (Switchboard stale), 6210 (Kamino reserve), size guards, etc.
//!
//! This probe finds every account our maintenance_health flags liquidatable at
//! FRESH on-chain prices (staleness-gated), sims the single-leg liquidate, and
//! tallies the true codes so we can see the real cause distribution.
//!
//! Usage: HELIUS_RPC=<url> [LIQUIDATOR_MA=…] [AUTHORITY=…] cargo run --release --bin mfi_reject_audit

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
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Pubkey {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"jsonParsed"}]}))
        .and_then(|v| v["result"]["value"]["owner"].as_str().and_then(|s| Pubkey::from_str(s).ok()))
        .unwrap_or_else(|| Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap())
}
fn is_debt_mint(m: &Pubkey) -> bool {
    let s = m.to_string();
    s == USDC_MINT || s == USDT_MINT || s == SOL_MINT
}

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
    let cap: usize = std::env::var("CAP").ok().and_then(|s| s.parse().ok()).unwrap_or(60);

    eprintln!("[audit] scanning marginfi group …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})).expect("scan");
    let accts: Vec<(Pubkey, MarginfiAccount)> = resp["result"].as_array().cloned().unwrap_or_default().iter()
        .filter_map(|e| Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?)))
        .filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();

    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &bank_raw { if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let slot = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0);
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    // PER-BANK staleness gate (the fix): each bank's on-chain oracle_max_age.
    let mut prices: PriceMap = HashMap::new();
    for (bank_pk, oracle_pk) in &oracle_of {
        let Some(raw) = oracle_raw.get(oracle_pk) else { continue };
        let max_age = banks.get(bank_pk).map(|b| b.oracle_max_age).unwrap_or(0);
        let max_stale = liq::max_stale_slots_for(max_age, liq::DEFAULT_MAX_SB_STALE_SLOTS);
        if let Some(usd) = liq::decode_oracle_price_fresh(raw, slot, max_stale) {
            prices.insert(*bank_pk, usd);
        }
    }

    // Accounts OUR maintenance_health flags liquidatable at FRESH on-chain price,
    // with a wired-debt leg (the ones try_arm would evaluate + reject).
    let mut flagged: Vec<(Pubkey, &MarginfiAccount, Pubkey, Pubkey)> = Vec::new();
    for (pk, a) in &accts {
        let h = liq::maintenance_health(a, &banks, &prices);
        if h.missing > 0 || !h.health.liquidatable() { continue; }
        let asset = a.balances.iter().filter(|b| b.asset_shares > 0.0)
            .max_by(|x, y| {
                let vx = banks.get(&x.bank_pk).and_then(|bk| prices.get(&x.bank_pk).map(|p| x.asset_shares*bk.asset_share_value/10f64.powi(bk.mint_decimals as i32)*p)).unwrap_or(0.0);
                let vy = banks.get(&y.bank_pk).and_then(|bk| prices.get(&y.bank_pk).map(|p| y.asset_shares*bk.asset_share_value/10f64.powi(bk.mint_decimals as i32)*p)).unwrap_or(0.0);
                vx.partial_cmp(&vy).unwrap_or(std::cmp::Ordering::Equal) });
        let debt = a.balances.iter().filter(|b| b.liability_shares > 0.0)
            .find(|b| banks.get(&b.bank_pk).map(|bk| is_debt_mint(&bk.mint)).unwrap_or(false));
        if let (Some(c), Some(d)) = (asset, debt) { flagged.push((*pk, a, c.bank_pk, d.bank_pk)); }
    }
    eprintln!("[audit] {} accounts our maintenance_health flags LIQUIDATABLE at fresh on-chain price (with a wired-debt leg)\n", flagged.len());

    let mut tally: HashMap<String, u32> = HashMap::new();
    let mut examples: HashMap<String, String> = HashMap::new();
    for (pk, a, asset_bank, liab_bank) in flagged.iter().take(cap) {
        let abk = &banks[asset_bank];
        let bal = a.balances.iter().find(|b| b.bank_pk == *asset_bank).unwrap();
        let seize = ((bal.asset_shares * abk.asset_share_value) * 0.02) as u64;
        let tp = mint_owner(&endpoint, &abk.mint);
        let Some(gate) = gate_tx_b64(&authority, &liquidator_ma, &tp, pk, a, *asset_bank, *liab_bank, seize, &oracle_of) else { continue };
        let sim = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[gate, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
        let res = sim.as_ref().map(|v| &v["result"]["value"]);
        let err = res.map(|r| &r["err"]);
        let key = match err {
            Some(e) if e.is_null() => "null → FIREABLE (real!)".to_string(),
            Some(e) => {
                let ie = e.get("InstructionError");
                let idx = ie.and_then(|x| x.get(0)).and_then(|x| x.as_u64());
                let code = ie.and_then(|x| x.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
                match (idx, code) {
                    (Some(1), Some(6068)) => "6068 HealthyAccount  (our math DISAGREES w/ chain at same price → BUG)".to_string(),
                    (Some(1), Some(6049)) => "6049 SwitchboardStalePrice (oracle stale — detection issue)".to_string(),
                    (Some(1), Some(6210)) => "6210 KaminoReserveValidation".to_string(),
                    (Some(1), Some(c)) => format!("in-liquidate Custom({c})"),
                    (Some(i), c) => format!("ix {i} Custom({c:?}) — WIRING?"),
                    _ => format!("other: {e}"),
                }
            }
            None => "rpc-error/no-result".to_string(),
        };
        *tally.entry(key.clone()).or_default() += 1;
        examples.entry(key).or_insert_with(|| pk.to_string());
    }

    println!("\n═══ REJECT-CODE DISTRIBUTION (why flagged accounts don't fire) ═══");
    let mut rows: Vec<_> = tally.into_iter().collect();
    rows.sort_by(|a, b| b.1.cmp(&a.1));
    for (k, n) in &rows {
        println!("  {n:3}  {k}");
        println!("        e.g. {}", examples.get(k).map(|s| s.as_str()).unwrap_or(""));
    }
    println!("\nKEY: 6068 = our health math over-flags vs the chain (a real logic bug to fix).");
    println!("     6049 = stale oracle (detection; the generous 5000-slot gate lets some through).");
    println!("     null = a genuinely fireable account we should have taken.");
}
