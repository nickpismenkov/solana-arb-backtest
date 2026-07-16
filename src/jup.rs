//! Jupiter swap API client (lite-api.jup.ag, keyless) — quote + composable
//! swap instructions for ARBITRARY mint pairs. Built for the liquidation fire
//! path (seized collateral → debt token can be any mint, unlike the arb path's
//! fixed pool basket). The arb hot path keeps its direct Orca/Ray builders (no
//! HTTP hop); liquidations are block-granularity, so a quote round-trip at
//! build time is affordable.

use anyhow::{anyhow, Result};
use solana_instruction::{AccountMeta, Instruction};
use solana_message::AddressLookupTableAccount;
use solana_pubkey::Pubkey;
use std::str::FromStr;
use std::time::Duration;

/// API base. Defaults to the keyless lite-api (rate-limited); override with
/// JUP_API_BASE to a Pro endpoint (e.g. https://api.jup.ag) under heavy load —
/// running several executors hammers lite-api and gets 429'd.
fn api_base() -> String {
    std::env::var("JUP_API_BASE").unwrap_or_else(|_| "https://lite-api.jup.ag".into())
}
fn quote_url() -> String { format!("{}/swap/v1/quote", api_base()) }
fn swap_ix_url() -> String { format!("{}/swap/v1/swap-instructions", api_base()) }

/// Jupiter API key (paid api.jup.ag tier). When set, sent as `x-api-key` on every
/// request — lifts the keyless lite-api rate limit that otherwise 429s live fires.
fn api_key() -> Option<String> { std::env::var("JUP_API_KEY").ok().filter(|k| !k.is_empty()) }

/// GET with exponential backoff on 429 / 5xx (the lite-api throttles under load).
fn get_json_retry(url: &str) -> Result<serde_json::Value> {
    let mut delay = 150u64;
    for attempt in 0..5 {
        let req = ureq::get(url);
        let req = match api_key() { Some(k) => req.set("x-api-key", &k), None => req };
        match req.call() {
            Ok(r) => return Ok(r.into_json()?),
            Err(ureq::Error::Status(code, _)) if (code == 429 || code >= 500) && attempt < 4 => {
                std::thread::sleep(Duration::from_millis(delay)); delay *= 2;
            }
            Err(ureq::Error::Status(code, r)) => return Err(anyhow!("jup GET {code}: {}", r.into_string().unwrap_or_default())),
            Err(e) => return Err(e.into()),
        }
    }
    Err(anyhow!("jup GET: exhausted retries (429/5xx)"))
}

/// POST with the same backoff.
fn post_json_retry(url: &str, body: &serde_json::Value) -> Result<serde_json::Value> {
    let mut delay = 150u64;
    for attempt in 0..5 {
        let req = ureq::post(url);
        let req = match api_key() { Some(k) => req.set("x-api-key", &k), None => req };
        match req.send_json(body.clone()) {
            Ok(r) => return Ok(r.into_json()?),
            Err(ureq::Error::Status(code, _)) if (code == 429 || code >= 500) && attempt < 4 => {
                std::thread::sleep(Duration::from_millis(delay)); delay *= 2;
            }
            Err(ureq::Error::Status(code, r)) => return Err(anyhow!("jup POST {code}: {}", r.into_string().unwrap_or_default())),
            Err(e) => return Err(e.into()),
        }
    }
    Err(anyhow!("jup POST: exhausted retries (429/5xx)"))
}

/// A quoted swap ready to splice into our own v0 transaction.
pub struct SwapPlan {
    /// setup + swap + cleanup, in order. Jupiter's compute-budget ixs are
    /// dropped — the enclosing tx owns its budget.
    pub instructions: Vec<Instruction>,
    pub alt_addresses: Vec<Pubkey>,
    pub quoted_out: u64,
    /// Jupiter's own slippage floor (min-out for ExactIn); the fire path's real
    /// guard is repay_all, this just reverts earlier/cheaper.
    pub min_out: u64,
}

/// ExactIn quote. `max_accounts` bounds route complexity so the swap fits in a
/// tx that already carries the flashloan + liquidate accounts.
pub fn quote(
    input_mint: &Pubkey,
    output_mint: &Pubkey,
    amount_in: u64,
    slippage_bps: u32,
    max_accounts: usize,
) -> Result<serde_json::Value> {
    let url = format!(
        "{}?inputMint={input_mint}&outputMint={output_mint}&amount={amount_in}\
         &slippageBps={slippage_bps}&swapMode=ExactIn&maxAccounts={max_accounts}", quote_url()
    );
    let v: serde_json::Value = get_json_retry(&url)?;
    if let Some(e) = v.get("error") {
        return Err(anyhow!("jup quote: {e}"));
    }
    if v.get("outAmount").and_then(|o| o.as_str()).is_none() {
        return Err(anyhow!("jup quote: no route ({v})"));
    }
    Ok(v)
}

fn decode_ix(v: &serde_json::Value) -> Result<Instruction> {
    use base64::Engine;
    let program_id = Pubkey::from_str(
        v["programId"].as_str().ok_or_else(|| anyhow!("ix missing programId"))?,
    )?;
    let mut accounts = Vec::new();
    for a in v["accounts"].as_array().into_iter().flatten() {
        accounts.push(AccountMeta {
            pubkey: Pubkey::from_str(a["pubkey"].as_str().ok_or_else(|| anyhow!("acct missing pubkey"))?)?,
            is_signer: a["isSigner"].as_bool().unwrap_or(false),
            is_writable: a["isWritable"].as_bool().unwrap_or(false),
        });
    }
    let data = base64::engine::general_purpose::STANDARD
        .decode(v["data"].as_str().unwrap_or(""))?;
    Ok(Instruction { program_id, accounts, data })
}

/// Turn a quote into instructions signable by `user`. `wrap_sol` only matters
/// when a leg is native SOL: the fire path swaps token ATAs directly (false,
/// marginfi withdraw lands wSOL in the wSOL ATA); a wallet-balance swap needs
/// the wrap (true).
pub fn swap_instructions(quote: &serde_json::Value, user: &Pubkey, wrap_sol: bool) -> Result<SwapPlan> {
    let body = serde_json::json!({
        "quoteResponse": quote,
        "userPublicKey": user.to_string(),
        "wrapAndUnwrapSol": wrap_sol,
        "dynamicComputeUnitLimit": false,
    });
    let v: serde_json::Value = post_json_retry(&swap_ix_url(), &body)?;
    if v.get("swapInstruction").map(|s| s.is_object()) != Some(true) {
        return Err(anyhow!("jup swap-instructions: {v}"));
    }
    let mut instructions = Vec::new();
    for ix in v["setupInstructions"].as_array().into_iter().flatten() {
        instructions.push(decode_ix(ix)?);
    }
    instructions.push(decode_ix(&v["swapInstruction"])?);
    if v["cleanupInstruction"].is_object() {
        instructions.push(decode_ix(&v["cleanupInstruction"])?);
    }
    let alt_addresses = v["addressLookupTableAddresses"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|a| a.as_str().and_then(|s| Pubkey::from_str(s).ok()))
        .collect();
    let quoted_out = quote["outAmount"].as_str().and_then(|s| s.parse().ok()).unwrap_or(0);
    let min_out = quote["otherAmountThreshold"].as_str().and_then(|s| s.parse().ok()).unwrap_or(0);
    Ok(SwapPlan { instructions, alt_addresses, quoted_out, min_out })
}

/// Fetch + decode the plan's lookup tables so the caller can compile a v0
/// message (addresses start at byte 56 of an ALT account).
pub fn fetch_alts(endpoint: &str, addrs: &[Pubkey]) -> Result<Vec<AddressLookupTableAccount>> {
    use base64::Engine;
    if addrs.is_empty() {
        return Ok(Vec::new());
    }
    let strs: Vec<String> = addrs.iter().map(|k| k.to_string()).collect();
    let v: serde_json::Value = ureq::post(endpoint)
        .send_json(serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]}))?
        .into_json()?;
    let mut out = Vec::new();
    for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
        let data = acc
            .get("data")
            .and_then(|d| d.get(0))
            .and_then(|s| s.as_str())
            .and_then(|s| base64::engine::general_purpose::STANDARD.decode(s).ok())
            .ok_or_else(|| anyhow!("ALT {} not found", addrs[i]))?;
        out.push(crate::arb::load_alt(&addrs[i].to_string(), &data));
    }
    Ok(out)
}
