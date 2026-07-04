//! Shred swap decoder + Address Lookup Table resolution. Turns a raw
//! VersionedTransaction (from a shred) into structured swaps on our pools:
//! which venue, direction (sell/buy the base asset), and amount. ALT resolution
//! also recovers swaps that reference a pool only via a lookup table — the ones
//! our static-key match in the ShredStream feed misses.

use std::collections::HashMap;

use solana_message::compiled_instruction::CompiledInstruction;
use solana_message::VersionedMessage;
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;

pub const ORCA_PROGRAM: &str = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc";
pub const RAY_CLMM_PROGRAM: &str = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK";

use crate::pools::pair;
use std::str::FromStr;

// Anchor "global:<name>" sighashes. Both Orca & Raydium CLMM use `swap` and
// `swap_v2`; venue is disambiguated by program id, not discriminator.
const DISC_SWAP: [u8; 8] = [0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8];
const DISC_SWAP_V2: [u8; 8] = [0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dir {
    SellBase, // base in → price down
    BuyBase,  // base out → price up
}

#[derive(Clone, Debug)]
pub struct SwapInfo {
    pub venue: &'static str,
    pub pool: String,
    pub dir: Dir,
    pub amount: u64,        // the instruction's `amount` arg (raw)
    pub amount_is_input: bool, // exact-input vs exact-output
    pub kind: &'static str, // "swap" | "swapV2"
}

/// Caches decoded Address Lookup Tables (pubkey → its address list). ALTs are
/// effectively static, so the cache warms fast and later txns are instant.
#[derive(Default)]
pub struct AltCache {
    rpc: String,
    tables: HashMap<Pubkey, Vec<Pubkey>>,
    last_fetch: Option<std::time::Instant>,
}

impl AltCache {
    pub fn new(rpc: impl Into<String>) -> Self {
        Self {
            rpc: rpc.into(),
            tables: HashMap::new(),
            last_fetch: None,
        }
    }

    /// Fully resolved account keys for a message: static keys, then ALT
    /// writables (per lookup order), then ALT readonlys — matching Solana's
    /// account index layout. Returns None if any referenced ALT can't be
    /// fetched/decoded.
    pub fn resolve_keys(&mut self, msg: &VersionedMessage) -> Option<Vec<Pubkey>> {
        let mut keys: Vec<Pubkey> = msg.static_account_keys().to_vec();
        let lookups = match msg {
            VersionedMessage::V0(m) => &m.address_table_lookups,
            VersionedMessage::Legacy(_) => return Some(keys), // no ALTs
        };
        let mut writable = Vec::new();
        let mut readonly = Vec::new();
        for lookup in lookups {
            let table = self.get_table(&lookup.account_key)?;
            for &i in &lookup.writable_indexes {
                writable.push(*table.get(i as usize)?);
            }
            for &i in &lookup.readonly_indexes {
                readonly.push(*table.get(i as usize)?);
            }
        }
        keys.extend(writable);
        keys.extend(readonly);
        Some(keys)
    }

    /// Lightweight pool-touch check for the trigger feed: static keys first (no
    /// RPC), then ALT-referenced addresses by index (cached table, no big
    /// alloc). Returns the matching venue. NOTE: cold ALTs block on an RPC
    /// fetch — fine for measurement (cache warms fast); production would
    /// pre-load the hot ALTs instead of fetching in the hot path.
    pub fn touches_pool(
        &mut self,
        msg: &VersionedMessage,
        pools: &[(Pubkey, &'static str)],
    ) -> Option<&'static str> {
        for k in msg.static_account_keys() {
            for (p, v) in pools {
                if k == p {
                    return Some(v);
                }
            }
        }
        if let VersionedMessage::V0(m) = msg {
            for lookup in &m.address_table_lookups {
                if !self.ensure_table(&lookup.account_key) {
                    continue;
                }
                let table = &self.tables[&lookup.account_key];
                for &i in lookup.writable_indexes.iter().chain(&lookup.readonly_indexes) {
                    if let Some(addr) = table.get(i as usize) {
                        for (p, v) in pools {
                            if addr == p {
                                return Some(v);
                            }
                        }
                    }
                }
            }
        }
        None
    }

    fn ensure_table(&mut self, key: &Pubkey) -> bool {
        if self.tables.contains_key(key) {
            return true;
        }
        // Cap cold-ALT fetches at ~10/s. Failed fetches aren't cached, so
        // without this a rate-limited RPC triggers a self-sustaining retry
        // flood off the txn firehose, starving every other consumer of the
        // endpoint (e.g. the executor's blockhash refresh). Hot ALTs recur
        // constantly, so the cache still warms within seconds.
        if let Some(t) = self.last_fetch {
            if t.elapsed() < std::time::Duration::from_millis(100) {
                return false;
            }
        }
        self.last_fetch = Some(std::time::Instant::now());
        if let Some(data) = fetch_account_data(&self.rpc, key) {
            // ALT: LookupTableMeta is 56 bytes; addresses follow, 32 bytes each.
            if data.len() >= 56 {
                let addrs: Vec<Pubkey> = data[56..]
                    .chunks_exact(32)
                    .map(|c| Pubkey::try_from(c).unwrap())
                    .collect();
                self.tables.insert(*key, addrs);
                return true;
            }
        }
        false
    }

    fn get_table(&mut self, key: &Pubkey) -> Option<Vec<Pubkey>> {
        if self.ensure_table(key) {
            self.tables.get(key).cloned()
        } else {
            None
        }
    }
}

/// Decode all swaps on our pools in a transaction, given its resolved keys.
pub fn decode_swaps(txn: &VersionedTransaction, keys: &[Pubkey]) -> Vec<SwapInfo> {
    let cfg = pair();
    let orca_pool = Pubkey::from_str(&cfg.orca_pool).expect("bad ORCA_POOL");
    let ray_pool = Pubkey::from_str(&cfg.ray_pool).expect("bad RAY_CLMM_POOL");
    let orca_prog = Pubkey::from_str_const(ORCA_PROGRAM);
    let ray_prog = Pubkey::from_str_const(RAY_CLMM_PROGRAM);
    let ray_vault0 = Pubkey::from_str(&cfg.ray_vault0).expect("bad RAY_VAULT0");

    let instructions: &[CompiledInstruction] = match &txn.message {
        VersionedMessage::V0(m) => &m.instructions,
        VersionedMessage::Legacy(m) => &m.instructions,
    };

    let mut out = Vec::new();
    for ix in instructions {
        let program = match keys.get(ix.program_id_index as usize) {
            Some(p) => *p,
            None => continue,
        };
        let venue = if program == orca_prog {
            "Orca"
        } else if program == ray_prog {
            "Raydium"
        } else {
            continue;
        };
        if ix.data.len() < 8 {
            continue;
        }
        let disc: [u8; 8] = ix.data[..8].try_into().unwrap();
        let kind = if disc == DISC_SWAP {
            "swap"
        } else if disc == DISC_SWAP_V2 {
            "swapV2"
        } else {
            continue;
        };

        // Resolve this instruction's accounts to pubkeys.
        let ix_keys: Vec<Pubkey> = ix
            .accounts
            .iter()
            .filter_map(|&a| keys.get(a as usize).copied())
            .collect();

        let (pool, dir) = if venue == "Orca" {
            if !ix_keys.contains(&orca_pool) {
                continue;
            }
            // swap args: amount u64, thresh u64, sqrtLimit u128, amtSpecIsInput bool, aToB bool
            if ix.data.len() < 42 {
                continue;
            }
            // NOTE: assumes mintA is the base asset (true for the default
            // SOL/USDC pool; verify per pair) → A→B = sell base.
            let a_to_b = ix.data[41] != 0;
            (cfg.orca_pool.clone(), if a_to_b { Dir::SellBase } else { Dir::BuyBase })
        } else {
            if !ix_keys.contains(&ray_pool) {
                continue;
            }
            // Raydium CLMM swap accounts: input_vault at index 5.
            let input_vault = match ix_keys.get(5) {
                Some(v) => *v,
                None => continue,
            };
            let dir = if input_vault == ray_vault0 {
                Dir::SellBase
            } else {
                Dir::BuyBase
            };
            (cfg.ray_pool.clone(), dir)
        };

        let amount = u64::from_le_bytes(ix.data[8..16].try_into().unwrap());
        // Orca: amountSpecifiedIsInput @40; Raydium CLMM: isBaseInput @40.
        let amount_is_input = ix.data.get(40).map(|&b| b != 0).unwrap_or(true);
        out.push(SwapInfo {
            venue,
            pool,
            dir,
            amount,
            amount_is_input,
            kind,
        });
    }
    out
}

fn fetch_account_data(rpc: &str, key: &Pubkey) -> Option<Vec<u8>> {
    let body = serde_json::json!({
        "jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
        "params": [key.to_string(), {"encoding": "base64"}]
    });
    let resp: serde_json::Value = ureq::post(rpc)
        .send_json(body)
        .ok()?
        .into_json()
        .ok()?;
    let b64 = resp["result"]["value"]["data"][0].as_str()?;
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(b64).ok()
}
