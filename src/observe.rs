//! Observability for the executor — append-only JSONL ledgers + realized-P&L
//! readback + alerts. STRICTLY off the hot path: all writes happen after a
//! bundle is submitted; realized P&L is read on a later poll. Mirrors the
//! design proven in the (now-retired) JS loop.

use serde::Serialize;
use std::fs::{create_dir_all, OpenOptions};
use std::io::Write;

fn append<T: Serialize>(dir: &str, file: &str, row: &T) {
    let _ = create_dir_all(dir);
    if let Ok(mut f) = OpenOptions::new().create(true).append(true).open(format!("{dir}/{file}")) {
        if let Ok(line) = serde_json::to_string(row) {
            let _ = writeln!(f, "{line}");
        }
    }
}

/// Every evaluated trigger — the denominator (why we did/didn't fire).
pub fn log_decision<T: Serialize>(dir: &str, row: &T) {
    append(dir, "decisions.jsonl", row);
}

/// Every fired bundle — the source of truth (quoted, then resolved P&L).
pub fn log_trade<T: Serialize>(dir: &str, row: &T) {
    append(dir, "trades.jsonl", row);
}

/// Realized USDC delta of the fee payer across a landed tx (actual result, not
/// the quote). None if the tx isn't on chain yet.
pub fn realized_usdc(rpc: &str, signature: &str, owner: &str) -> Option<f64> {
    const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
        "params":[signature,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]});
    let v: serde_json::Value = ureq::post(rpc).send_json(body).ok()?.into_json().ok()?;
    let meta = &v["result"]["meta"];
    if meta.is_null() {
        return None;
    }
    let sum = |key: &str| -> f64 {
        meta[key]
            .as_array()
            .into_iter()
            .flatten()
            .filter(|b| b["mint"] == USDC && b["owner"] == owner)
            .filter_map(|b| b["uiTokenAmount"]["uiAmount"].as_f64())
            .sum()
    };
    Some(sum("postTokenBalances") - sum("preTokenBalances"))
}

/// Fire-and-forget alert to a webhook (Slack/Discord/generic) if set.
pub fn alert(webhook: &Option<String>, key: &str, message: &str) {
    eprintln!("[ALERT:{key}] {message}");
    if let Some(url) = webhook {
        let _ = ureq::post(url).send_json(serde_json::json!({"text": format!("arb [{key}] {message}")}));
    }
}
