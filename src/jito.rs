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
