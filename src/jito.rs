//! Jito bundle submission via the block engine: getTipAccounts + sendBundle,
//! plus a SOL tip-transfer instruction helper. Region-matched to the box
//! (Amsterdam) by default. Used only when we go live — building/holding a
//! bundle costs nothing; you pay only if it lands (and a guarded arb that
//! isn't profitable reverts → the bundle never lands).

use anyhow::{anyhow, Result};
use solana_pubkey::Pubkey;
use std::str::FromStr;

pub fn default_block_engine() -> String {
    std::env::var("JITO_BLOCK_ENGINE")
        .unwrap_or_else(|_| "https://amsterdam.mainnet.block-engine.jito.wtf".to_string())
}

/// Fetch the current Jito tip accounts (pick one at random per bundle).
pub fn get_tip_accounts(block_engine: &str) -> Result<Vec<Pubkey>> {
    let url = format!("{block_engine}/api/v1/bundles");
    let body = serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTipAccounts","params":[]});
    let resp: serde_json::Value = ureq::post(&url).send_json(body)?.into_json()?;
    let arr = resp["result"]
        .as_array()
        .ok_or_else(|| anyhow!("getTipAccounts: no result ({resp})"))?;
    Ok(arr
        .iter()
        .filter_map(|s| s.as_str())
        .filter_map(|s| Pubkey::from_str(s).ok())
        .collect())
}

/// Submit an atomic bundle (base64-encoded txs). Returns the bundle id.
pub fn send_bundle(block_engine: &str, txs_b64: &[String]) -> Result<String> {
    let url = format!("{block_engine}/api/v1/bundles");
    let body = serde_json::json!({
        "jsonrpc":"2.0","id":1,"method":"sendBundle",
        "params":[txs_b64, {"encoding":"base64"}]
    });
    let resp: serde_json::Value = match ureq::post(&url).send_json(body) {
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
