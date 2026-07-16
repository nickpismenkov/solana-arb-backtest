//! Jito bundle submission via the block engine: getTipAccounts + sendBundle,
//! plus a SOL tip-transfer instruction helper. Region-matched to the box
//! (Amsterdam) by default. Used only when we go live — building/holding a
//! bundle costs nothing; you pay only if it lands (and a guarded arb that
//! isn't profitable reverts → the bundle never lands).

use anyhow::{anyhow, Result};
use base64::Engine;
use solana_pubkey::Pubkey;
use std::str::FromStr;
use std::sync::OnceLock;
use std::time::Duration;

pub fn default_block_engine() -> String {
    std::env::var("JITO_BLOCK_ENGINE")
        .unwrap_or_else(|_| "https://amsterdam.mainnet.block-engine.jito.wtf".to_string())
}

/// Shared HTTP agent with connection pooling / keep-alive, so submits reuse a
/// warm TLS connection instead of paying a fresh handshake (~several ms) every
/// time. The single biggest submit-latency win for a co-located box. Agent is
/// cheap to clone (Arc inside).
fn agent() -> &'static ureq::Agent {
    static AGENT: OnceLock<ureq::Agent> = OnceLock::new();
    AGENT.get_or_init(|| {
        ureq::AgentBuilder::new()
            .timeout_connect(Duration::from_secs(2))
            .timeout(Duration::from_secs(5))
            .max_idle_connections_per_host(4)
            .build()
    })
}

/// Fetch the current Jito tip accounts (pick one at random per bundle).
pub fn get_tip_accounts(block_engine: &str) -> Result<Vec<Pubkey>> {
    let url = format!("{block_engine}/api/v1/bundles");
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTipAccounts","params":[]});
    let resp: serde_json::Value = agent().post(&url).send_json(body)?.into_json()?;
    let arr = resp["result"]
        .as_array()
        .ok_or_else(|| anyhow!("getTipAccounts: no result ({resp})"))?;
    Ok(arr
        .iter()
        .filter_map(|s| s.as_str())
        .filter_map(|s| Pubkey::from_str(s).ok())
        .collect())
}

/// Submit a single signed tx via Helius Sender (dual-routes to validators +
/// Jito for fast landing; no 1/sec Jito-unauth cap). Requires a tip ≥0.0002 SOL
/// as a transfer to a Jito tip account inside the tx (we already include one).
/// skipPreflight=true — Sender blasts, doesn't simulate; our guard is the real
/// check. Returns the signature.
pub fn send_sender(sender_url: &str, tx_b64: &str) -> Result<String> {
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendTransaction",
        "params":[tx_b64, {"encoding":"base64","skipPreflight":true,"maxRetries":0}]});
    let resp: serde_json::Value = match agent().post(sender_url).send_json(body) {
        Ok(r) => r.into_json()?,
        Err(ureq::Error::Status(code, r)) => {
            let b = r.into_string().unwrap_or_default();
            return Err(anyhow!("sender HTTP {code}: {b}"));
        }
        Err(e) => return Err(e.into()),
    };
    if let Some(e) = resp.get("error").filter(|e| !e.is_null()) {
        return Err(anyhow!("sender error: {e}"));
    }
    resp["result"].as_str().map(|s| s.to_string()).ok_or_else(|| anyhow!("sender: no signature"))
}

/// Post-hoc status of a submitted bundle: "Landed", "Failed" (dropped — e.g.
/// our guard would revert, or we lost the race), "Pending", or "Invalid"
/// (expired/never seen). Off the hot path — call seconds after firing.
pub fn bundle_status(block_engine: &str, bundle_id: &str) -> Option<String> {
    let url = format!("{block_engine}/api/v1/getInflightBundleStatuses");
    let body = serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"getInflightBundleStatuses",
        "params":[[bundle_id]]
    });
    let resp: serde_json::Value = ureq::post(&url).send_json(body).ok()?.into_json().ok()?;
    resp["result"]["value"][0]["status"].as_str().map(|s| s.to_string())
}

/// send_bundle with regional fallback: each Jito block engine rate-limits
/// independently, so when the primary (Amsterdam, lowest latency) answers 429
/// the bundle falls through to the next region instead of being dropped — a
/// 10h census counted 270 sendBundle 429s that killed 45 of 73 real fire
/// attempts during a burst. Non-retryable errors (bad bundle) abort at once.
/// Override the fallback list via JITO_FALLBACK_ENGINES (csv).
pub fn send_bundle_rotate(primary: &str, txs_b64: &[String]) -> Result<String> {
    let extra = std::env::var("JITO_FALLBACK_ENGINES").unwrap_or_else(|_| concat!(
        "https://frankfurt.mainnet.block-engine.jito.wtf,",
        "https://london.mainnet.block-engine.jito.wtf,",
        "https://mainnet.block-engine.jito.wtf").into());
    let mut engines = vec![primary.to_string()];
    engines.extend(extra.split(',').map(|s| s.trim().to_string()).filter(|s| !s.is_empty() && s != primary));
    let mut last: Option<anyhow::Error> = None;
    for ep in &engines {
        match send_bundle(ep, txs_b64) {
            Ok(r) => return Ok(r),
            Err(e) => {
                let retryable = { let s = e.to_string(); s.contains("HTTP 429") || s.contains("HTTP 5") };
                last = Some(e);
                if !retryable { break; }
            }
        }
    }
    Err(last.unwrap_or_else(|| anyhow!("send_bundle_rotate: no engines")))
}

/// Submit an atomic bundle (base64-encoded txs). Returns the bundle id.
/// JITO_BUNDLE_ENCODING=base58 re-encodes and submits via Jito's default
/// (base58) path instead — diagnostic for base64-path drops.
pub fn send_bundle(block_engine: &str, txs_b64: &[String]) -> Result<String> {
    let url = format!("{block_engine}/api/v1/bundles");
    let use_b58 = std::env::var("JITO_BUNDLE_ENCODING").map(|v| v == "base58").unwrap_or(false);
    let body = if use_b58 {
        let txs_b58: Vec<String> = txs_b64
            .iter()
            .map(|b64| {
                let raw = base64::engine::general_purpose::STANDARD.decode(b64).unwrap_or_default();
                bs58::encode(raw).into_string()
            })
            .collect();
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendBundle","params":[txs_b58]})
    } else {
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"sendBundle","params":[txs_b64, {"encoding":"base64"}]})
    };
    let resp: serde_json::Value = match agent().post(&url).send_json(body) {
        Ok(r) => r.into_json()?,
        // Jito puts the real rejection reason in the error response body —
        // surface it instead of just the status line.
        Err(ureq::Error::Status(code, r)) => {
            let body = r.into_string().unwrap_or_default();
            return Err(anyhow!("sendBundle HTTP {code}: {body}"));
        }
        Err(e) => return Err(e.into()),
    };
    if let Some(e) = resp.get("error").filter(|e| !e.is_null()) {
        return Err(anyhow!("sendBundle error: {e}"));
    }
    Ok(resp["result"].as_str().unwrap_or_default().to_string())
}
