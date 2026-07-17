//! Self-crank instruction builders (step 4 of the pipeline): turn a parsed
//! Hermes update (pyth_accumulator) into the on-chain ix sequence that posts a
//! fresh price to the SHARED sponsored PriceUpdateV2 feed marginfi reads.
//!
//! Derived from a REAL mainnet sponsored-feed crank (the marginfi/Kamino
//! lesson) — setup tx 2hkWPh62… + fire tx 4vYXYqdN…, same buffer keypair:
//!
//!   SETUP tx: system createAccount(space = 46 + vaa_len, owner = Wormhole-verify)
//!             · init_encoded_vaa · write_encoded_vaa(index 0, first ~720B)
//!   FIRE  tx: write_encoded_vaa(tail) · verify_encoded_vaa_v1(guardian_set)
//!             · push-wrapper update_price_feed{PostUpdateParams, shard, feed_id}
//!               (CPIs receiver post_update → writes the sponsored feed PDA,
//!                whose write_authority is itself — the permissionless pattern)
//!             · close_encoded_vaa (rent back)
//!
//! The two-tx split exists because the guardian-signed VAA (~952B for one
//! Hermes blob) can't fit a single 1232B tx next to the 396B update ix. One
//! encoded-VAA buffer can serve several update ixs (the VAA commits to the
//! merkle root of ALL feeds in the blob); only the last fire tx should close.
//!
//! Observed rent for space=998 was 7,836,960 lamports = (space+128)*6960 —
//! reclaimed by close_encoded_vaa.

use crate::pyth_accumulator::MerkleUpdate;
use anyhow::{anyhow, Result};
use solana_instruction::{AccountMeta, Instruction};
use solana_pubkey::Pubkey;
use std::str::FromStr;

/// Wormhole verification program the Pyth receiver trusts (encoded-VAA flow).
pub const WORMHOLE_VERIFY: &str = "HDwcJBJXjL9FpJ7UBsYBtaDjsBUhuLCUYoz3zr8SWWaQ";
/// Pyth pull-oracle receiver (post_update lives here; wrapper CPIs into it).
pub const PYTH_RECEIVER: &str = "rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ";
/// Pyth push wrapper — the ONLY writer of the shared sponsored feeds.
pub const PUSH_ORACLE: &str = "pythWSnswVUd12oZpeFP8e9CVaEqJg25g1Vtc2biRsT";
/// Receiver config PDA (seed "config"), pinned from the captured tx.
pub const RECEIVER_CONFIG: &str = "DaWUKXCyXsnzcvLUyeJRWou8KTn7XtadgTsdhJ6RHS7b";
const SYSTEM: &str = "11111111111111111111111111111111";

// VERIFIED discriminators (sha256("global:<name>")[..8], matched against the
// captured crank — see pyth_crank_decode).
const DISC_INIT_ENCODED_VAA: [u8; 8] = [0xd1, 0xc1, 0xad, 0x19, 0x5b, 0xca, 0xb5, 0xda];
const DISC_WRITE_ENCODED_VAA: [u8; 8] = [0xc7, 0xd0, 0x6e, 0xb1, 0x96, 0x4c, 0x76, 0x2a];
const DISC_VERIFY_ENCODED_VAA_V1: [u8; 8] = [0x67, 0x38, 0xb1, 0xe5, 0xf0, 0x67, 0x44, 0x49];
const DISC_CLOSE_ENCODED_VAA: [u8; 8] = [0x30, 0xdd, 0xae, 0xc6, 0xe7, 0x07, 0x98, 0x26];
const DISC_UPDATE_PRICE_FEED: [u8; 8] = [0x1c, 0x09, 0x5d, 0x96, 0x56, 0x99, 0xbc, 0x73];

/// First-chunk size for the setup tx (observed cranker used ~720; the tail +
/// verify + update + close must fit the 1232B fire tx).
pub const SETUP_CHUNK: usize = 720;

fn pk(s: &str) -> Pubkey { Pubkey::from_str(s).unwrap() }

/// EncodedVaa account size: 46B header (disc + status + write_authority +
/// version + len) + the raw VAA. Observed: 952B VAA → space 998.
pub fn encoded_vaa_space(vaa_len: usize) -> u64 { 46 + vaa_len as u64 }

/// Rent-exempt minimum, matching the observed createAccount exactly.
pub fn rent_lamports(space: u64) -> u64 { (space + 128) * 6960 }

/// Guardian-set index a VAA was signed under (u32 BE right after the version).
pub fn guardian_set_index(vaa: &[u8]) -> Option<u32> {
    Some(u32::from_be_bytes(vaa.get(1..5)?.try_into().ok()?))
}

/// Guardian-set PDA of the Wormhole-verify program.
pub fn guardian_set(index: u32) -> Pubkey {
    Pubkey::find_program_address(
        &[b"GuardianSet", &index.to_be_bytes()],
        &pk(WORMHOLE_VERIFY),
    ).0
}

/// Receiver treasury PDA (any id 0..=255 works; ids spread write contention).
pub fn treasury(id: u8) -> Pubkey {
    Pubkey::find_program_address(&[b"treasury", &[id]], &pk(PYTH_RECEIVER)).0
}

/// Sponsored-feed PDA the push wrapper writes: seeds [shard_le, feed_id].
pub fn sponsored_feed(shard: u16, feed_id: &[u8; 32]) -> Pubkey {
    Pubkey::find_program_address(&[&shard.to_le_bytes(), feed_id], &pk(PUSH_ORACLE)).0
}

/// System createAccount for the encoded-VAA buffer (buffer must co-sign).
pub fn create_encoded_vaa_account_ix(payer: &Pubkey, buffer: &Pubkey, vaa_len: usize) -> Instruction {
    let space = encoded_vaa_space(vaa_len);
    let mut data = Vec::with_capacity(52);
    data.extend_from_slice(&0u32.to_le_bytes()); // CreateAccount tag
    data.extend_from_slice(&rent_lamports(space).to_le_bytes());
    data.extend_from_slice(&space.to_le_bytes());
    data.extend_from_slice(pk(WORMHOLE_VERIFY).as_ref());
    Instruction {
        program_id: pk(SYSTEM),
        accounts: vec![
            AccountMeta::new(*payer, true),
            AccountMeta::new(*buffer, true),
        ],
        data,
    }
}

pub fn init_encoded_vaa_ix(write_authority: &Pubkey, buffer: &Pubkey) -> Instruction {
    Instruction {
        program_id: pk(WORMHOLE_VERIFY),
        accounts: vec![
            AccountMeta::new_readonly(*write_authority, true),
            AccountMeta::new(*buffer, false),
        ],
        data: DISC_INIT_ENCODED_VAA.to_vec(),
    }
}

/// write_encoded_vaa { index: u32 (byte offset), data: Vec<u8> }.
pub fn write_encoded_vaa_ix(write_authority: &Pubkey, buffer: &Pubkey, index: u32, chunk: &[u8]) -> Instruction {
    let mut data = Vec::with_capacity(16 + chunk.len());
    data.extend_from_slice(&DISC_WRITE_ENCODED_VAA);
    data.extend_from_slice(&index.to_le_bytes());
    data.extend_from_slice(&(chunk.len() as u32).to_le_bytes());
    data.extend_from_slice(chunk);
    Instruction {
        program_id: pk(WORMHOLE_VERIFY),
        accounts: vec![
            AccountMeta::new_readonly(*write_authority, true),
            AccountMeta::new(*buffer, false),
        ],
        data,
    }
}

pub fn verify_encoded_vaa_v1_ix(write_authority: &Pubkey, buffer: &Pubkey, guardian_set: &Pubkey) -> Instruction {
    Instruction {
        program_id: pk(WORMHOLE_VERIFY),
        accounts: vec![
            AccountMeta::new_readonly(*write_authority, true),
            AccountMeta::new(*buffer, false),
            AccountMeta::new_readonly(*guardian_set, false),
        ],
        data: DISC_VERIFY_ENCODED_VAA_V1.to_vec(),
    }
}

/// Rent goes back to the write authority.
pub fn close_encoded_vaa_ix(write_authority: &Pubkey, buffer: &Pubkey) -> Instruction {
    Instruction {
        program_id: pk(WORMHOLE_VERIFY),
        accounts: vec![
            AccountMeta::new(*write_authority, true),
            AccountMeta::new(*buffer, false),
        ],
        data: DISC_CLOSE_ENCODED_VAA.to_vec(),
    }
}

/// Push-wrapper update_price_feed(PostUpdateParams { merkle_price_update
/// { message: Vec<u8>, proof: Vec<[u8;20]> }, treasury_id: u8 }, shard: u16,
/// feed_id: [u8;32]) — 396B observed for an 85B message + 13-hash proof.
/// Converts the accumulator's wire proof (u8 count + count×20B) to borsh
/// (u32 count + hashes). Returns None on a malformed proof.
pub fn update_price_feed_ix(
    payer: &Pubkey,
    encoded_vaa: &Pubkey,
    update: &MerkleUpdate,
    shard: u16,
    treasury_id: u8,
) -> Option<Instruction> {
    let feed_id = update.feed_id()?;
    let n = *update.proof.first()? as usize;
    let hashes = update.proof.get(1..1 + n * 20)?;
    let mut data = Vec::with_capacity(8 + 4 + update.message.len() + 4 + hashes.len() + 35);
    data.extend_from_slice(&DISC_UPDATE_PRICE_FEED);
    data.extend_from_slice(&(update.message.len() as u32).to_le_bytes());
    data.extend_from_slice(&update.message);
    data.extend_from_slice(&(n as u32).to_le_bytes());
    data.extend_from_slice(hashes);
    data.push(treasury_id);
    data.extend_from_slice(&shard.to_le_bytes());
    data.extend_from_slice(&feed_id);
    Some(Instruction {
        program_id: pk(PUSH_ORACLE),
        accounts: vec![
            AccountMeta::new(*payer, true),
            AccountMeta::new_readonly(pk(PYTH_RECEIVER), false),
            AccountMeta::new_readonly(*encoded_vaa, false),
            AccountMeta::new_readonly(pk(RECEIVER_CONFIG), false),
            AccountMeta::new(treasury(treasury_id), false),
            AccountMeta::new(sponsored_feed(shard, &feed_id), false),
            AccountMeta::new_readonly(pk(SYSTEM), false),
        ],
        data,
    })
}

/// The full two-tx crank for one Hermes blob: setup (create+init+first chunk)
/// and fire (tail+verify+update per feed+close). `updates` may hold several
/// feeds — one VAA proves them all; each gets its own update ix in the fire tx
/// if it fits (each adds ~430B; more than 2 feeds needs splitting upstream).
pub struct CrankIxs {
    pub setup: Vec<Instruction>,
    pub fire: Vec<Instruction>,
}

pub fn build_crank_ixs(
    payer: &Pubkey,
    buffer: &Pubkey,
    vaa: &[u8],
    updates: &[MerkleUpdate],
    shard: u16,
    treasury_id: u8,
) -> Option<CrankIxs> {
    let gs = guardian_set(guardian_set_index(vaa)?);
    let head = vaa.get(..SETUP_CHUNK.min(vaa.len()))?;
    let setup = vec![
        create_encoded_vaa_account_ix(payer, buffer, vaa.len()),
        init_encoded_vaa_ix(payer, buffer),
        write_encoded_vaa_ix(payer, buffer, 0, head),
    ];
    let mut fire = Vec::with_capacity(3 + updates.len());
    if vaa.len() > head.len() {
        fire.push(write_encoded_vaa_ix(payer, buffer, head.len() as u32, &vaa[head.len()..]));
    }
    fire.push(verify_encoded_vaa_v1_ix(payer, buffer, &gs));
    for u in updates {
        fire.push(update_price_feed_ix(payer, buffer, u, shard, treasury_id)?);
    }
    fire.push(close_encoded_vaa_ix(payer, buffer));
    Some(CrankIxs { setup, fire })
}

// ── Transaction-level assembly (what the executor bundles ahead of the
//    liquidate tx) ──────────────────────────────────────────────────────────

use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_message::{v0, VersionedMessage};
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;

/// CU ceilings from the captured crank: setup ran in ~5.4k, fire in ~402k
/// (verify_encoded_vaa_v1 does 13 secp recoveries).
pub const SETUP_CU: u32 = 30_000;
pub const FIRE_CU: u32 = 500_000;

fn cu_limit_ix(units: u32) -> Instruction {
    let mut data = vec![2u8];
    data.extend_from_slice(&units.to_le_bytes());
    Instruction {
        program_id: pk("ComputeBudget111111111111111111111111111111"),
        accounts: vec![],
        data,
    }
}

/// The two crank txs plus the ephemeral buffer keypair that must co-sign the
/// setup (createAccount). Build fresh per fire — the VAA inside is only as
/// fresh as the Hermes blob it came from.
pub struct CrankTxs {
    pub setup: VersionedTransaction,
    pub fire: VersionedTransaction,
    pub buffer: Keypair,
}

impl CrankTxs {
    /// Stamp a recent blockhash and sign: setup = [payer, buffer], fire = [payer].
    pub fn stamp_and_sign(&mut self, payer: &Keypair, blockhash: Hash) {
        self.setup.message.set_recent_blockhash(blockhash);
        self.fire.message.set_recent_blockhash(blockhash);
        let sm = self.setup.message.serialize();
        self.setup.signatures[0] = payer.sign_message(&sm);
        self.setup.signatures[1] = self.buffer.sign_message(&sm);
        self.fire.signatures[0] = payer.sign_message(&self.fire.message.serialize());
    }

    /// (setup, fire) as base64 — the encoding sendBundle/simulateBundle take.
    pub fn to_b64(&self) -> Result<(String, String)> {
        use base64::Engine;
        let e = |tx: &VersionedTransaction| -> Result<String> {
            Ok(base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx)?))
        };
        Ok((e(&self.setup)?, e(&self.fire)?))
    }
}

/// Compile the two-tx crank for `updates` (usually one feed) with placeholder
/// signatures. `blockhash` may be default for replace-blockhash simulation;
/// stamp_and_sign before a live send.
pub fn build_crank_txs(
    payer: &Pubkey,
    vaa: &[u8],
    updates: &[MerkleUpdate],
    shard: u16,
    treasury_id: u8,
    blockhash: Hash,
) -> Result<CrankTxs> {
    build_crank_txs_tipped(payer, vaa, updates, shard, treasury_id, blockhash, None)
}

/// As `build_crank_txs`, but appends the Jito tip transfer to the SETUP tx.
/// In a bundle every tx lands or none do, so the tip is equally binding in the
/// setup tx as in the liquidate tx — and putting it here frees ~45B from the
/// liquidate tx, which is the one that brushes the 1232B wire limit on a 2-hop
/// swap. The setup tx (~1150B) has the headroom.
pub fn build_crank_txs_tipped(
    payer: &Pubkey,
    vaa: &[u8],
    updates: &[MerkleUpdate],
    shard: u16,
    treasury_id: u8,
    blockhash: Hash,
    tip: Option<(Pubkey, u64)>,
) -> Result<CrankTxs> {
    let buffer = Keypair::new();
    let mut ixs = build_crank_ixs(payer, &buffer.pubkey(), vaa, updates, shard, treasury_id)
        .ok_or_else(|| anyhow!("crank ix build failed (malformed VAA/update)"))?;
    if let Some((tip_to, lamports)) = tip {
        if lamports > 0 { ixs.setup.push(crate::arb::transfer_ix(*payer, tip_to, lamports)); }
    }
    let compile = |cu: u32, body: &[Instruction], n_sigs: usize| -> Result<VersionedTransaction> {
        let mut all = vec![cu_limit_ix(cu)];
        all.extend_from_slice(body);
        let msg = v0::Message::try_compile(payer, &all, &[], blockhash)
            .map_err(|e| anyhow!("compile crank tx: {e}"))?;
        Ok(VersionedTransaction {
            signatures: vec![solana_signature::Signature::default(); n_sigs],
            message: VersionedMessage::V0(msg),
        })
    };
    Ok(CrankTxs {
        setup: compile(SETUP_CU, &ixs.setup, 2)?,
        fire: compile(FIRE_CU, &ixs.fire, 1)?,
        buffer,
    })
}

/// Multi-feed crank: post SEVERAL fresh Pyth feeds in ONE bundle so a pure
/// account whose non-collateral Pyth legs are ALSO stale on-chain still lands
/// (marginfi 6019/stale-reads any stale obs oracle, not just the collateral).
/// One update ix (~430B) + verify blows the 1232B fire tx past the limit at 2
/// feeds, so we SPLIT: one update per fire tx. The encoded-VAA buffer persists
/// across bundle txs (same slot), so verify runs once in the first fire and each
/// later fire just posts its update from the already-verified buffer; the last
/// fire closes it. Bundle = [setup(+tip), fire0…fireN-1, liquidate] — Jito's
/// 5-tx cap → at most 3 feeds (setup + 3 + liq), plenty for real pure accounts.
pub struct CrankMulti {
    pub setup: VersionedTransaction,
    pub fires: Vec<VersionedTransaction>,
    pub buffer: Keypair,
}

impl CrankMulti {
    pub fn stamp_and_sign(&mut self, payer: &Keypair, blockhash: Hash) {
        self.setup.message.set_recent_blockhash(blockhash);
        let sm = self.setup.message.serialize();
        self.setup.signatures[0] = payer.sign_message(&sm);
        self.setup.signatures[1] = self.buffer.sign_message(&sm);
        for f in &mut self.fires {
            f.message.set_recent_blockhash(blockhash);
            f.signatures[0] = payer.sign_message(&f.message.serialize());
        }
    }
    /// All crank txs as base64, in bundle order (setup first, then the fires).
    pub fn to_b64(&self) -> Result<Vec<String>> {
        use base64::Engine;
        let e = |tx: &VersionedTransaction| -> Result<String> {
            Ok(base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx)?))
        };
        let mut v = vec![e(&self.setup)?];
        for f in &self.fires { v.push(e(f)?); }
        Ok(v)
    }
}

pub fn build_crank_txs_multi(
    payer: &Pubkey,
    vaa: &[u8],
    updates: &[MerkleUpdate],
    shard: u16,
    treasury_id: u8,
    blockhash: Hash,
    tip: Option<(Pubkey, u64)>,
) -> Result<CrankMulti> {
    if updates.is_empty() { return Err(anyhow!("multi-crank: no updates")); }
    let buffer = Keypair::new();
    let bpk = buffer.pubkey();
    let gs = guardian_set(guardian_set_index(vaa).ok_or_else(|| anyhow!("vaa guardian-set index"))?);
    let head = vaa.get(..SETUP_CHUNK.min(vaa.len())).ok_or_else(|| anyhow!("vaa head"))?;
    let compile = |cu: u32, body: Vec<Instruction>, n_sigs: usize| -> Result<VersionedTransaction> {
        let mut all = vec![cu_limit_ix(cu)];
        all.extend(body);
        let msg = v0::Message::try_compile(payer, &all, &[], blockhash).map_err(|e| anyhow!("compile crank tx: {e}"))?;
        Ok(VersionedTransaction { signatures: vec![solana_signature::Signature::default(); n_sigs], message: VersionedMessage::V0(msg) })
    };
    // setup: create + init encoded-VAA buffer + write the VAA head (+ optional tip).
    let mut setup_ixs = vec![
        create_encoded_vaa_account_ix(payer, &bpk, vaa.len()),
        init_encoded_vaa_ix(payer, &bpk),
        write_encoded_vaa_ix(payer, &bpk, 0, head),
    ];
    if let Some((to, lam)) = tip { if lam > 0 { setup_ixs.push(crate::arb::transfer_ix(*payer, to, lam)); } }
    let setup = compile(SETUP_CU, setup_ixs, 2)?;
    // fires: one update per tx. fire[0] writes the VAA tail + verifies; the last
    // closes the buffer. Later fires only pay ~60k CU (a bare update).
    let n = updates.len();
    let mut fires = Vec::with_capacity(n);
    for (i, u) in updates.iter().enumerate() {
        let mut ixs: Vec<Instruction> = Vec::new();
        if i == 0 {
            if vaa.len() > head.len() { ixs.push(write_encoded_vaa_ix(payer, &bpk, head.len() as u32, &vaa[head.len()..])); }
            ixs.push(verify_encoded_vaa_v1_ix(payer, &bpk, &gs));
        }
        ixs.push(update_price_feed_ix(payer, &bpk, u, shard, treasury_id).ok_or_else(|| anyhow!("multi-crank update ix"))?);
        if i == n - 1 { ixs.push(close_encoded_vaa_ix(payer, &bpk)); }
        fires.push(compile(if i == 0 { FIRE_CU } else { 60_000 }, ixs, 1)?);
    }
    Ok(CrankMulti { setup, fires, buffer })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every PDA derivation is asserted against addresses captured from the
    /// real mainnet crank (setup 2hkWPh62…, fire 4vYXYqdN…).
    #[test]
    fn pdas_match_observed_crank() {
        // Guardian set 7 (the captured VAA's index) — account in verify ix.
        assert_eq!(guardian_set(7).to_string(), "6GaHgiaQg9Pg346xHq9m7vQ9rJtnH83gQKqJoiAxQa7D");
        // Treasury observed in the update ix — some id must derive it.
        let t = (0u8..=255).find(|&id| treasury(id).to_string() == "8hQfT7SVhkCrzUSgBq6u2wYEt1sH3xmofZ5ss3YaydZW");
        assert!(t.is_some(), "no treasury id derives the observed treasury");
        // Receiver config PDA (seed "config").
        let cfg = Pubkey::find_program_address(&[b"config"], &pk(PYTH_RECEIVER)).0;
        assert_eq!(cfg.to_string(), RECEIVER_CONFIG);
        // Sponsored feeds: SOL (ef0d8b6f…) → 7UVimff…, PENGU-ish (d82183dd…) →
        // F2VfCy… — both from the captured cranks; find the shard.
        let sol: [u8; 32] = hex("ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d");
        let other: [u8; 32] = hex("d82183dd487bef3208a227bb25d748930db58862c5121198e723ed0976eb92b7");
        let shard = (0u16..64).find(|&s| sponsored_feed(s, &sol).to_string() == "7UVimffxr9ow1uXYxsr4LHAcV58mLzhmwaeKvJ1pjLiE");
        assert!(shard.is_some(), "no shard derives the observed SOL sponsored feed");
        let s = shard.unwrap();
        assert_eq!(sponsored_feed(s, &other).to_string(), "F2VfCymdNQiCa8Vyg5E7BwEv9UPwfm8cVN6eqQLqXiGo",
            "second observed feed disagrees on shard {s}");
        eprintln!("treasury_id={} shard={}", t.unwrap(), s);
    }

    /// Rent formula reproduces the observed createAccount lamports exactly.
    #[test]
    fn rent_matches_observed() {
        assert_eq!(encoded_vaa_space(952), 998);
        assert_eq!(rent_lamports(998), 7_836_960);
    }

    fn hex(s: &str) -> [u8; 32] {
        let mut out = [0u8; 32];
        for i in 0..32 { out[i] = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap(); }
        out
    }
}
