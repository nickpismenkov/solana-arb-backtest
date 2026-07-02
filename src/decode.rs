//! Shred swap decoder + Address Lookup Table resolution. Turns a raw
//! VersionedTransaction (from a shred) into structured swaps on our pools:
//! which venue, direction (sell/buy SOL), and amount. ALT resolution also
//! recovers swaps that reference a pool only via a lookup table — the ones our
//! static-key match in the ShredStream feed misses.

use std::collections::HashMap;

use solana_message::compiled_instruction::CompiledInstruction;
use solana_message::VersionedMessage;
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;

pub const ORCA_PROGRAM: &str = "whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc";
pub const RAY_CLMM_PROGRAM: &str = "CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK";

use crate::pools::{ORCA_POOL, RAY_CLMM_POOL};
// Raydium CLMM vault0 = SOL (mint0). Input vault == this => SOL is being sold.
const RAY_VAULT0_SOL: &str = "4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A";

// Anchor "global:<name>" sighashes. Both Orca & Raydium CLMM use `swap` and
// `swap_v2`; venue is disambiguated by program id, not discriminator.
const DISC_SWAP: [u8; 8] = [0xf8, 0xc6, 0x9e, 0x91, 0xe1, 0x75, 0x87, 0xc8];
const DISC_SWAP_V2: [u8; 8] = [0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dir {
    SellSol, // SOL in → price down
    BuySol,  // SOL out → price up
}

#[derive(Clone, Debug)]
pub struct SwapInfo {
    pub venue: &'static str,
    pub pool: &'static str,
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
}

impl AltCache {
    pub fn new(rpc: impl Into<String>) -> Self {
        Self {
            rpc: rpc.into(),
            tables: HashMap::new(),
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

    fn get_table(&mut self, key: &Pubkey) -> Option<Vec<Pubkey>> {
        if let Some(t) = self.tables.get(key) {
            return Some(t.clone());
        }
        let data = fetch_account_data(&self.rpc, key)?;
        // ALT: LookupTableMeta is 56 bytes; addresses follow, 32 bytes each.
        if data.len() < 56 {
            return None;
        }
        let addrs: Vec<Pubkey> = data[56..]
            .chunks_exact(32)
            .map(|c| Pubkey::try_from(c).unwrap())
            .collect();
        self.tables.insert(*key, addrs.clone());
        Some(addrs)
    }
}

/// Decode all swaps on our pools in a transaction, given its resolved keys.
pub fn decode_swaps(txn: &VersionedTransaction, keys: &[Pubkey]) -> Vec<SwapInfo> {
    let orca_pool = Pubkey::from_str_const(ORCA_POOL);
    let ray_pool = Pubkey::from_str_const(RAY_CLMM_POOL);
    let orca_prog = Pubkey::from_str_const(ORCA_PROGRAM);
    let ray_prog = Pubkey::from_str_const(RAY_CLMM_PROGRAM);
    let ray_vault0 = Pubkey::from_str_const(RAY_VAULT0_SOL);

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
            let a_to_b = ix.data[41] != 0; // mintA is SOL → A→B = sell SOL
            (ORCA_POOL, if a_to_b { Dir::SellSol } else { Dir::BuySol })
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
                Dir::SellSol
            } else {
                Dir::BuySol
            };
            (RAY_CLMM_POOL, dir)
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
