//! pump.fun / PumpSwap recon + decoders — **Phase 1 (measure-first)**.
//!
//! Everything here is derived from CAPTURED REAL MAINNET TRANSACTIONS, never
//! from memory (repo doctrine). Program IDs, event discriminators, and byte
//! offsets are each verified against ≥2 live txs; the unit tests at the bottom
//! pin the layouts to real captured bytes so a future pump upgrade that shifts a
//! field trips a test instead of silently corrupting the collector.
//!
//! This module is fully self-contained and shares NO state with the liquidation
//! engine. It is pure observation: nothing here signs or submits a transaction.
//!
//! ── What was verified (see the PR body for the exact signatures) ────────────
//! * Bonding-curve program `6EF8…F6P` — owns every `BondingCurve` PDA and emits
//!   the anchor self-CPI event logs (`Program data: …`) we decode.
//! * PumpSwap AMM `pAMM…fXEA` — the graduated venue; a bonding curve migrates
//!   into a PumpSwap pool at completion.
//! * The current mainnet program is the **Token-2022 variant** (mints/curve token
//!   accounts live under `TokenzQd…`), and the instruction set has grown V2/V3
//!   variants (Create/CreateV2, Buy/BuyV2, Sell/SellV2, MigrateV2). We therefore
//!   decode the **anchor event logs**, whose layout is stable across those
//!   instruction variants, rather than per-instruction discriminators.
//!
//! ── Anchor event self-CPI logs ──────────────────────────────────────────────
//! pump emits structured events as base64 in `Program data:` log lines. The
//! first 8 bytes are the anchor event discriminator (`sha256("event:<Name>")`).
//! We match on those to route Create / Trade / Migrate.

use solana_pubkey::Pubkey;

// ── Program IDs (verified: getAccountInfo owner/executable + real txs) ────────

/// pump.fun bonding-curve program. VERIFIED: it is the `owner` of every
/// `BondingCurve` account we fetched, and every create/buy/sell/migrate tx we
/// pulled is a top-level invoke of this program.
pub const PUMP_PROGRAM: &str = "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P";

/// PumpSwap AMM program (post-graduation venue). VERIFIED: `executable=true`,
/// owned by the BPF upgradeable loader, and it is account #10 of the `MigrateV2`
/// instruction (the pool the curve migrates into).
pub const PUMPSWAP_AMM: &str = "pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA";

/// pump.fun fee program (`GetFees` CPI seen in every trade). Informational.
pub const PUMP_FEE_PROGRAM: &str = "pfeeUxB6jkeY1Hxd7CsFCAjcbHA9rWtchMGdZ6VojVZ";

/// Migration authority — the signer of `MigrateV2`. Its transaction history is,
/// in effect, the list of graduations. VERIFIED across multiple migrate txs.
pub const MIGRATION_AUTHORITY: &str = "39azUYFWPz3VHgKCf3VChUwbpURdCHRxjWVowf5jUJjg";

/// PDA seed for a token's bonding-curve account: `["bonding-curve", mint]`.
/// VERIFIED: find_program_address(["bonding-curve", mint], PUMP_PROGRAM) equals
/// the `bonding_curve` field of the CreateEvent (see test).
pub const BONDING_CURVE_SEED: &[u8] = b"bonding-curve";

/// pump.fun tokens are minted with 6 decimals and a 1e15 raw total supply
/// (= 1,000,000,000 whole tokens). VERIFIED from CreateEvent.token_total_supply.
pub const PUMP_TOKEN_DECIMALS: u32 = 6;

// ── Anchor event discriminators (first 8 bytes of a `Program data:` blob) ─────

/// `sha256("event:TradeEvent")[..8]` — emitted on every buy and sell.
pub const TRADE_EVENT_DISC: [u8; 8] = [0xbd, 0xdb, 0x7f, 0xd3, 0x4e, 0xe6, 0x61, 0xee];
/// `sha256("event:CreateEvent")[..8]` — emitted on a new token launch.
pub const CREATE_EVENT_DISC: [u8; 8] = [0x1b, 0x72, 0xa9, 0x4d, 0xde, 0xeb, 0x63, 0x76];
/// Migrate/complete event — emitted inside the `MigrateV2` tx. Discriminator
/// captured live; only its `mint` field (byte offset 50) is decoded, see below.
pub const MIGRATE_EVENT_DISC: [u8; 8] = [0xb1, 0x31, 0x0c, 0xd2, 0xa0, 0x76, 0xa7, 0x74];

// ── Reserve → price helpers ──────────────────────────────────────────────────

/// Price of one **raw** token unit in **lamports**, straight from the constant-
/// product reserves (`virtual_sol_reserves / virtual_token_reserves`).
pub fn price_lamports_per_raw_token(virtual_sol: u64, virtual_token: u64) -> f64 {
    if virtual_token == 0 {
        return 0.0;
    }
    virtual_sol as f64 / virtual_token as f64
}

/// Price of one **whole** token in **SOL**, accounting for the 9 lamport decimals
/// of SOL and `token_decimals` of the token (6 for pump). This is the number you
/// compare across launches for the "peak multiple" census.
pub fn price_in_sol(virtual_sol: u64, virtual_token: u64, token_decimals: u32) -> f64 {
    if virtual_token == 0 {
        return 0.0;
    }
    let sol = virtual_sol as f64 / 1e9;
    let tokens = virtual_token as f64 / 10f64.powi(token_decimals as i32);
    sol / tokens
}

/// Raw token units received for paying `sol_in` lamports **into** the bonding
/// curve (pre-fee, pure constant-product). `virtual_sol`/`virtual_token` are the
/// curve's virtual reserves *before* the trade.
///
/// pump.fun's curve is a constant product `k = vsol * vtoken`. Paying `sol_in`
/// raises `vsol` to `vsol + sol_in`, so tokens out = `vtoken - k/(vsol+sol_in)`
/// = `vtoken * sol_in / (vsol + sol_in)`. VERIFIED to the lamport against a real
/// captured dev-buy (see test `curve_buy_matches_captured_trade`): the pump
/// `TradeEvent.sol_amount` is exactly the SOL that enters the curve, and its
/// `token_amount` equals this function's output. The pump/creator fee is charged
/// **separately, on top** of `sol_amount` (125 bps in the captured tx) — model it
/// outside this pure curve function.
pub fn curve_buy_tokens_out(virtual_sol: u64, virtual_token: u64, sol_in: u64) -> u64 {
    if sol_in == 0 {
        return 0;
    }
    let den = virtual_sol as u128 + sol_in as u128;
    if den == 0 {
        return 0;
    }
    // Result is strictly < virtual_token, so it always fits in u64.
    ((virtual_token as u128 * sol_in as u128) / den) as u64
}

/// Lamports of SOL received for selling `tokens_in` raw token units **into** the
/// curve (pre-fee, pure constant-product). Symmetric to [`curve_buy_tokens_out`]:
/// sol out = `vsol * tokens_in / (vtoken + tokens_in)`. The trading fee is charged
/// separately on the SOL received — apply it outside.
pub fn curve_sell_sol_out(virtual_sol: u64, virtual_token: u64, tokens_in: u64) -> u64 {
    if tokens_in == 0 {
        return 0;
    }
    let den = virtual_token as u128 + tokens_in as u128;
    if den == 0 {
        return 0;
    }
    // Result is strictly < virtual_sol, so it always fits in u64.
    ((virtual_sol as u128 * tokens_in as u128) / den) as u64
}

/// Derive a token's bonding-curve PDA from its mint.
pub fn bonding_curve_pda(mint: &Pubkey) -> Pubkey {
    let program: Pubkey = PUMP_PROGRAM.parse().expect("const program id");
    Pubkey::find_program_address(&[BONDING_CURVE_SEED, mint.as_ref()], &program).0
}

// ── BondingCurve account layout ──────────────────────────────────────────────
// VERIFIED against a live account whose reserves matched a TradeEvent from the
// same slot exactly (all four reserves + supply). Total account data = 151 bytes
// (8 disc + 5×u64 + 1 bool + 32 creator + trailing zero pad).
//
//   offset  field
//   0       8-byte account discriminator (17 b7 f8 37 60 d8 ac 60)
//   8       virtual_token_reserves : u64
//   16      virtual_sol_reserves   : u64
//   24      real_token_reserves    : u64
//   32      real_sol_reserves      : u64
//   40      token_total_supply     : u64
//   48      complete               : bool  (1 = graduated / migrating)
//   49      creator                : pubkey (32)

/// A decoded `BondingCurve` account — the on-chain price/graduation state.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BondingCurve {
    pub virtual_token_reserves: u64,
    pub virtual_sol_reserves: u64,
    pub real_token_reserves: u64,
    pub real_sol_reserves: u64,
    pub token_total_supply: u64,
    /// True once the curve has filled and is graduating to PumpSwap.
    pub complete: bool,
    pub creator: Pubkey,
}

/// Account discriminator for a `BondingCurve` (anchor `sha256("account:BondingCurve")`).
pub const BONDING_CURVE_ACCOUNT_DISC: [u8; 8] = [0x17, 0xb7, 0xf8, 0x37, 0x60, 0xd8, 0xac, 0x60];

impl BondingCurve {
    /// Decode raw account data. Returns `None` if the discriminator does not
    /// match or the buffer is short (i.e. it is not a BondingCurve account).
    pub fn decode(data: &[u8]) -> Option<Self> {
        if data.len() < 81 || data[..8] != BONDING_CURVE_ACCOUNT_DISC {
            return None;
        }
        Some(Self {
            virtual_token_reserves: u64_le(data, 8)?,
            virtual_sol_reserves: u64_le(data, 16)?,
            real_token_reserves: u64_le(data, 24)?,
            real_sol_reserves: u64_le(data, 32)?,
            token_total_supply: u64_le(data, 40)?,
            complete: data[48] != 0,
            creator: pubkey_at(data, 49)?,
        })
    }

    /// Price of one whole token in SOL from the current virtual reserves.
    pub fn price_in_sol(&self) -> f64 {
        price_in_sol(
            self.virtual_sol_reserves,
            self.virtual_token_reserves,
            PUMP_TOKEN_DECIMALS,
        )
    }
}

// ── TradeEvent (buy / sell) ──────────────────────────────────────────────────
// VERIFIED against ≥2 real events (a Sell and a dev Buy). Core layout:
//   0    disc(8)                 40  sol_amount:u64
//   8    mint:pubkey(32)         48  token_amount:u64
//   56   is_buy:bool             57  user:pubkey(32)
//   89   timestamp:i64           97  virtual_sol_reserves:u64
//   105  virtual_token_reserves  113 real_sol_reserves:u64
//   121  real_token_reserves:u64 129 fee_recipient:pubkey(32)
//   161  fee_basis_points:u64    169 fee:u64
//   177  creator:pubkey(32)      209 creator_fee_basis_points:u64
//   217  creator_fee:u64         (further fields: volume accounting + a "buy"/
//                                 "sell" string — not needed, left undecoded)

/// A decoded buy or sell on the bonding curve.
#[derive(Clone, Debug)]
pub struct TradeEvent {
    pub mint: Pubkey,
    /// Lamports of SOL that moved (paid on a buy, received on a sell).
    pub sol_amount: u64,
    /// Raw token units that moved.
    pub token_amount: u64,
    pub is_buy: bool,
    /// The trader.
    pub user: Pubkey,
    pub timestamp: i64,
    pub virtual_sol_reserves: u64,
    pub virtual_token_reserves: u64,
    pub real_sol_reserves: u64,
    pub real_token_reserves: u64,
    /// The token's creator (dev). Present in the current layout; `None` on the
    /// older/shorter variant. Useful for the dev-dump rug proxy.
    pub creator: Option<Pubkey>,
}

impl TradeEvent {
    /// Decode a `Program data:` blob whose first 8 bytes are `TRADE_EVENT_DISC`.
    pub fn decode(data: &[u8]) -> Option<Self> {
        if data.len() < 129 || data[..8] != TRADE_EVENT_DISC {
            return None;
        }
        let creator = if data.len() >= 209 {
            pubkey_at(data, 177)
        } else {
            None
        };
        Some(Self {
            mint: pubkey_at(data, 8)?,
            sol_amount: u64_le(data, 40)?,
            token_amount: u64_le(data, 48)?,
            is_buy: data[56] != 0,
            user: pubkey_at(data, 57)?,
            timestamp: i64_le(data, 89)?,
            virtual_sol_reserves: u64_le(data, 97)?,
            virtual_token_reserves: u64_le(data, 105)?,
            real_sol_reserves: u64_le(data, 113)?,
            real_token_reserves: u64_le(data, 121)?,
            creator,
        })
    }

    /// Whole-token price implied by this event's post-trade virtual reserves.
    pub fn price_in_sol(&self) -> f64 {
        price_in_sol(
            self.virtual_sol_reserves,
            self.virtual_token_reserves,
            PUMP_TOKEN_DECIMALS,
        )
    }
}

// ── CreateEvent (new launch) ─────────────────────────────────────────────────
// VERIFIED against a CreateV2 tx. Layout (three leading anchor strings, so it is
// variable-length up to `mint`):
//   0   disc(8)
//   8   name:string  symbol:string  uri:string    (each u32 len + bytes)
//   ..  mint:pubkey  bonding_curve:pubkey  user:pubkey(dev)  creator:pubkey
//   ..  timestamp:i64
//   ..  virtual_token_reserves:u64  virtual_sol_reserves:u64
//   ..  real_token_reserves:u64     token_total_supply:u64

/// A decoded new-token launch.
#[derive(Clone, Debug)]
pub struct CreateEvent {
    pub name: String,
    pub symbol: String,
    pub uri: String,
    pub mint: Pubkey,
    pub bonding_curve: Pubkey,
    /// Wallet that submitted the create (the "dev").
    pub user: Pubkey,
    /// The recorded creator (== `user` in every sample; distinct field on-chain).
    pub creator: Pubkey,
    pub timestamp: i64,
    pub virtual_token_reserves: u64,
    pub virtual_sol_reserves: u64,
    pub real_token_reserves: u64,
    pub token_total_supply: u64,
}

impl CreateEvent {
    /// Decode a `Program data:` blob whose first 8 bytes are `CREATE_EVENT_DISC`.
    pub fn decode(data: &[u8]) -> Option<Self> {
        if data.len() < 8 || data[..8] != CREATE_EVENT_DISC {
            return None;
        }
        let mut o = 8usize;
        let name = read_string(data, &mut o)?;
        let symbol = read_string(data, &mut o)?;
        let uri = read_string(data, &mut o)?;
        let mint = pubkey_at(data, o)?;
        o += 32;
        let bonding_curve = pubkey_at(data, o)?;
        o += 32;
        let user = pubkey_at(data, o)?;
        o += 32;
        let creator = pubkey_at(data, o)?;
        o += 32;
        let timestamp = i64_le(data, o)?;
        o += 8;
        let virtual_token_reserves = u64_le(data, o)?;
        o += 8;
        let virtual_sol_reserves = u64_le(data, o)?;
        o += 8;
        let real_token_reserves = u64_le(data, o)?;
        o += 8;
        let token_total_supply = u64_le(data, o)?;
        Some(Self {
            name,
            symbol,
            uri,
            mint,
            bonding_curve,
            user,
            creator,
            timestamp,
            virtual_token_reserves,
            virtual_sol_reserves,
            real_token_reserves,
            token_total_supply,
        })
    }
}

// ── Migrate event ────────────────────────────────────────────────────────────
// The MigrateV2 tx emits an event with `MIGRATE_EVENT_DISC`. Its full field
// layout is NOT fully decoded (fields before `mint` are not needed and left
// undocumented — being honest); the token `mint` sits at a fixed byte offset of
// 50, VERIFIED stable across 3 migrate events (all ending in "pump").

/// A decoded graduation. Only `mint` is extracted from the event; the new
/// PumpSwap pool is derivable from the migrate instruction accounts if needed.
#[derive(Clone, Debug)]
pub struct MigrateEvent {
    pub mint: Pubkey,
}

impl MigrateEvent {
    const MINT_OFFSET: usize = 50;
    /// Decode a `Program data:` blob whose first 8 bytes are `MIGRATE_EVENT_DISC`.
    pub fn decode(data: &[u8]) -> Option<Self> {
        if data.len() < Self::MINT_OFFSET + 32 || data[..8] != MIGRATE_EVENT_DISC {
            return None;
        }
        Some(Self {
            mint: pubkey_at(data, Self::MINT_OFFSET)?,
        })
    }
}

// ── Unified event ────────────────────────────────────────────────────────────

/// Any pump.fun event decoded from a `Program data:` log blob.
#[derive(Clone, Debug)]
pub enum PumpEvent {
    Create(CreateEvent),
    Trade(TradeEvent),
    Migrate(MigrateEvent),
}

impl PumpEvent {
    /// Route a raw event blob (already base64-decoded) by its discriminator.
    pub fn parse(data: &[u8]) -> Option<Self> {
        if data.len() < 8 {
            return None;
        }
        let disc: [u8; 8] = data[..8].try_into().ok()?;
        match disc {
            CREATE_EVENT_DISC => CreateEvent::decode(data).map(PumpEvent::Create),
            TRADE_EVENT_DISC => TradeEvent::decode(data).map(PumpEvent::Trade),
            MIGRATE_EVENT_DISC => MigrateEvent::decode(data).map(PumpEvent::Migrate),
            _ => None,
        }
    }

    /// Short tag used in the collector's JSONL `event_type` field.
    pub fn kind(&self) -> &'static str {
        match self {
            PumpEvent::Create(_) => "create",
            PumpEvent::Trade(t) if t.is_buy => "buy",
            PumpEvent::Trade(_) => "sell",
            PumpEvent::Migrate(_) => "migrate",
        }
    }
}

/// Decode the base64 payload of a `Program data:` log line into a `PumpEvent`.
/// Non-pump / unrecognised blobs return `None`.
pub fn parse_program_data_b64(b64: &str) -> Option<PumpEvent> {
    use base64::Engine;
    let bytes = base64::engine::general_purpose::STANDARD.decode(b64).ok()?;
    PumpEvent::parse(&bytes)
}

// ── little-endian read helpers ───────────────────────────────────────────────

fn u64_le(d: &[u8], o: usize) -> Option<u64> {
    Some(u64::from_le_bytes(d.get(o..o + 8)?.try_into().ok()?))
}
fn i64_le(d: &[u8], o: usize) -> Option<i64> {
    Some(i64::from_le_bytes(d.get(o..o + 8)?.try_into().ok()?))
}
fn pubkey_at(d: &[u8], o: usize) -> Option<Pubkey> {
    let b: [u8; 32] = d.get(o..o + 32)?.try_into().ok()?;
    Some(Pubkey::new_from_array(b))
}
/// Read an anchor `String` (u32 little-endian length prefix + UTF-8 bytes),
/// advancing `o`. Lossy on invalid UTF-8 (token names are arbitrary user input).
fn read_string(d: &[u8], o: &mut usize) -> Option<String> {
    let len = u32::from_le_bytes(d.get(*o..*o + 4)?.try_into().ok()?) as usize;
    *o += 4;
    let bytes = d.get(*o..*o + len)?;
    *o += len;
    Some(String::from_utf8_lossy(bytes).into_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::Engine;

    fn b64(s: &str) -> Vec<u8> {
        base64::engine::general_purpose::STANDARD.decode(s).unwrap()
    }

    // Captured live 2026-07-13. See module header / PR for signatures.
    const BONDING_CURVE_ACCT: &str = "F7f4N2DYrGD/z64uP88DAA+NUP0GAAAA/zec4q3QAgAP4SwBAAAAAACAxqR+jQMAAJ14QhGVBrWTXxU1diOOZq7XdbeEk0gWP7qHpqDuQoMIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==";
    // Sell TradeEvent from tx 3EyynvWEhik3cLjLUZjaUs7zmDTazCWBuKDUXpUAbbSBtGu3RBNu5JLYVZcq7Qk84SR2vv6vvtfHki8YUaQZnhrf
    const SELL_EVENT: &str = "vdt/007mYe53dojVRkYf9U7PwzqGhSiP2yB5IreUzC3Jf/If6LPeTwIBAAAAAAAASSqNAAAAAAAAqoV/wjyh+tVMxN+p2EipkwN1JdrpCkiRB99UT/NXyrVZblVqAAAAAA+NUP0GAAAA/8+uLj/PAwAP4SwBAAAAAP83nOKt0AIAjRgaDISfqTem80re0wge+VcAqssMm7PZCaS5FHUnpOtfAAAAAAAAAAMAAAAAAAAAnXhCEZUGtZNfFTV2I45mrtd1t4STSBY/uoemoO5CgwgeAAAAAAAAAAEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAAAAHNlbGwAAAAAAAAAAAAAAAAAAAAAAIgTAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACAQAAAAAAAA+NUP0GAAAAD+EsAQAAAAA=";
    // CreateEvent from tx 4DQVzRrgwJhoJMXjWh4sQPrJDhjQSVs6NXkXqYQzwybdN8vsZHfhDrHx6RFW97HUh6DQKfrbAQsDKAAdgKxFAkQY
    const CREATE_EVENT: &str = "G3KpTd7rY3YLAAAASk9SREFOIEdPQVQGAAAASk9SREFOQwAAAGh0dHBzOi8vaXBmcy5pby9pcGZzL1FtV0NwamJBUnZtTDNDS0I2d1E5NUpleXVqYlBENmRWVWZlanJXazZvOVZOU1EZ1kA78qeo0rHRRuCa5pFwXxXMUCwZhiqwa/LVMvJrz8bYHCJ2Knatm8LTF5G1k/7G67dC5zrMWjB94hsRpwZTtPfiMhEhESA+3yzSw6ojFRgT2CH0KyeqTqCPv6Hc14u09+IyESERID7fLNLDqiMVGBPYIfQrJ6pOoI+/odzXiyBvVWoAAAAAABDYR+PPAwAArCP8BgAAAAB4xftR0QIAAIDGpH6NAwAG3fbh7nWP3hhCXbzkbM3athr8TYO5DSf+vfko2KGL/AEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAArCP8BgAAAA==";
    // Buy TradeEvent (dev buy) from the same CreateV2 tx as CREATE_EVENT.
    const BUY_EVENT: &str = "vdt/007mYe4Z1kA78qeo0rHRRuCa5pFwXxXMUCwZhiqwa/LVMvJrzwGj4REAAAAAI6+ViakJAAABtPfiMhEhESA+3yzSw6ojFRgT2CH0KyeqTqCPv6Hc14sgb1VqAAAAAAFPBQ4HAAAA3WBCvjnGAwABo+ERAAAAAN3IL3KoxwIA6JMUH7GOnxV02BDheOGeMGBOMXWqLkoy38hgByfRBwlfAAAAAAAAANF8KwAAAAAAtPfiMhEhESA+3yzSw6ojFRgT2CH0KyeqTqCPv6Hc14seAAAAAAAAAKG7DQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAwAAAGJ1eQEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGj4REAAAAAAU8FDgcAAAABo+ERAAAAAA==";
    // MigrateV2 event from tx 3nNAWrzjUkuV56hHvczQpLCG24Ajv3E6BrG2WYvjz4sPWV36wsCcQiakvXjnTk8fAKFdazWMwVJZRSxfPiC6AVZ2
    const MIGRATE_EVENT: &str = "sTEM0qB2p3TTb1VqAAAAAAAAxuqgGtucU3HoGEx4cOunLOv2nAaQFGBNG1pAlfASdxI7uDBrxeg0ExPNVYen+8qZG9mI4yuPLrlzIJygfZysjwabiFf+q4GE+2h/Y0YYwDXaxDncGus7VZig8AAAAAABBgkACAGpLLwAABZ+5EIBAAAAAAgBqSy8AAAWfuRCAQAAAGQAAAAAAAAA2gzmfvYAAAB2DOZ+9gAAAP+WoJezDAZ3uIUJFNQQ7PtXkN7U6E3fcFAXikbjOvGSmh7qoT099JlVMURmXNHouIDtCTt/lr0P804o+ZTrTchMjSUEzU0l7K0uiMeCAjAG0LtfBmbI1MIvYYbuoeyzM912joX8bRGFC4K/wWbzHTZIze9r89cCJhrT2RDwMBf1pZrsrJ7Y7f0sUWMudtC8yqEQtSxU64vvEQwWh4cstyT9AQ==";

    #[test]
    fn bonding_curve_layout_matches_captured_account() {
        let bc = BondingCurve::decode(&b64(BONDING_CURVE_ACCT)).expect("decode");
        assert_eq!(bc.virtual_token_reserves, 1_072_295_203_229_695);
        assert_eq!(bc.virtual_sol_reserves, 30_019_718_415);
        assert_eq!(bc.real_token_reserves, 792_395_203_229_695);
        assert_eq!(bc.real_sol_reserves, 19_718_415);
        assert_eq!(bc.token_total_supply, 1_000_000_000_000_000);
        assert!(!bc.complete);
        assert_eq!(
            bc.creator.to_string(),
            "BbhNCDrtHwUiAjx9F5CUmQZsb8verXQ1AzUsACCdjXq1"
        );
    }

    #[test]
    fn bonding_curve_reserves_equal_trade_event_reserves() {
        // Same slot: the account state must equal the sell event's post-trade
        // reserves — this is what pins the offsets to reality.
        let bc = BondingCurve::decode(&b64(BONDING_CURVE_ACCT)).unwrap();
        let ev = TradeEvent::decode(&b64(SELL_EVENT)).unwrap();
        assert_eq!(bc.virtual_sol_reserves, ev.virtual_sol_reserves);
        assert_eq!(bc.virtual_token_reserves, ev.virtual_token_reserves);
        assert_eq!(bc.real_sol_reserves, ev.real_sol_reserves);
        assert_eq!(bc.real_token_reserves, ev.real_token_reserves);
    }

    #[test]
    fn sell_trade_event_decodes() {
        let ev = TradeEvent::decode(&b64(SELL_EVENT)).unwrap();
        assert!(!ev.is_buy);
        assert_eq!(ev.token_amount, 9_251_401);
        assert_eq!(ev.sol_amount, 258);
        assert_eq!(
            ev.mint.to_string(),
            "93LMDXJnQ6mZTVSoT86qxb5sEtzWpGmTUVpEKaqupump"
        );
        assert_eq!(
            ev.user.to_string(),
            "CUeNoQKZJN933yMunWg7eSsz3cpbYeMC19e8ppyVudxG"
        );
        assert!(ev.creator.is_some());
    }

    #[test]
    fn buy_trade_event_decodes() {
        let ev = TradeEvent::decode(&b64(BUY_EVENT)).unwrap();
        assert!(ev.is_buy);
        assert_eq!(
            ev.mint.to_string(),
            "2jrgGZTiLaWFbujkkqSyLHKadja8e7f61qJhKDuhpump"
        );
    }

    #[test]
    fn create_event_decodes_and_pda_matches() {
        let ev = CreateEvent::decode(&b64(CREATE_EVENT)).unwrap();
        assert_eq!(ev.name, "JORDAN GOAT");
        assert_eq!(ev.symbol, "JORDAN");
        assert_eq!(
            ev.mint.to_string(),
            "2jrgGZTiLaWFbujkkqSyLHKadja8e7f61qJhKDuhpump"
        );
        assert_eq!(
            ev.bonding_curve.to_string(),
            "EPCrTyzVKHYqvMD17t5h2gEZU5CcMAMt5bMuwc1nKRCN"
        );
        assert_eq!(
            ev.user.to_string(),
            "DBRcgABPRT9g9rE9UdRbqK55NSPzpVbogEH4H32AAke6"
        );
        // Canonical pump.fun launch reserves.
        assert_eq!(ev.virtual_sol_reserves, 30_000_000_000);
        assert_eq!(ev.virtual_token_reserves, 1_073_000_000_000_000);
        assert_eq!(ev.token_total_supply, 1_000_000_000_000_000);
        // The bonding_curve field must equal the derived PDA.
        assert_eq!(bonding_curve_pda(&ev.mint), ev.bonding_curve);
    }

    #[test]
    fn migrate_event_mint_at_offset_50() {
        let ev = MigrateEvent::decode(&b64(MIGRATE_EVENT)).unwrap();
        assert_eq!(
            ev.mint.to_string(),
            "527xDTvS2Nk727LdbzJfS8WeSYvaH8bnx8cYjXzTpump"
        );
    }

    #[test]
    fn event_router_dispatches_by_discriminator() {
        assert_eq!(parse_program_data_b64(CREATE_EVENT).unwrap().kind(), "create");
        assert_eq!(parse_program_data_b64(BUY_EVENT).unwrap().kind(), "buy");
        assert_eq!(parse_program_data_b64(SELL_EVENT).unwrap().kind(), "sell");
        assert_eq!(parse_program_data_b64(MIGRATE_EVENT).unwrap().kind(), "migrate");
    }

    #[test]
    fn curve_buy_matches_captured_trade() {
        // Pins the constant-product buy math to REAL data. The captured dev-buy
        // (BUY_EVENT) paid sol_amount=300_000_001 lamports INTO the canonical
        // launch curve (vsol=30e9, vtoken=1_073e9…) and received token_amount=
        // 10_623_762_411_299 raw tokens. Our function must reproduce it exactly.
        let ev = TradeEvent::decode(&b64(BUY_EVENT)).unwrap();
        let out = curve_buy_tokens_out(30_000_000_000, 1_073_000_000_000_000, ev.sol_amount);
        assert_eq!(out, ev.token_amount);
        assert_eq!(ev.token_amount, 10_623_762_411_299);
        // post-trade vsol == pre + sol_amount (the SOL really enters the curve).
        assert_eq!(ev.virtual_sol_reserves, 30_000_000_000 + ev.sol_amount);
    }

    #[test]
    fn curve_sell_is_buy_inverse_within_rounding() {
        // Buy `sol_in`, which moves the curve to (vsol+sol_in, vtok-toks). Selling
        // those tokens straight back into that POST-buy state recovers the SOL paid,
        // minus only integer-division dust (never more — no free money). (Selling
        // into the PRE-buy reserves instead would lose real curve convexity, ~3% on
        // this size — that is the round-trip slippage a quick flip actually eats.)
        let (vsol, vtoken) = (30_000_000_000u64, 1_073_000_000_000_000u64);
        let sol_in = 500_000_000u64;
        let toks = curve_buy_tokens_out(vsol, vtoken, sol_in);
        let post_vsol = vsol + sol_in;
        let post_vtok = vtoken - toks;
        let back = curve_sell_sol_out(post_vsol, post_vtok, toks);
        assert!(back <= sol_in, "sell must not exceed the buy: {back} > {sol_in}");
        assert!(sol_in - back <= 4, "round-trip dust too large: {}", sol_in - back);
    }

    #[test]
    fn curve_buy_has_price_impact() {
        // Bigger buys get strictly fewer tokens PER sol (slippage up the curve):
        // doubling the SOL in must return less than double the tokens.
        let (vsol, vtoken) = (30_000_000_000u64, 1_073_000_000_000_000u64);
        let small = curve_buy_tokens_out(vsol, vtoken, 1_000_000_000);
        let big = curve_buy_tokens_out(vsol, vtoken, 2_000_000_000);
        assert!(big < 2 * small, "no price impact: {big} >= 2*{small}");
    }

    #[test]
    fn price_in_sol_is_sane() {
        // ~30 SOL virtual / ~1.073e15 raw tokens (6 dec) ≈ 2.8e-8 SOL/token.
        let p = price_in_sol(30_000_000_000, 1_073_000_000_000_000, PUMP_TOKEN_DECIMALS);
        assert!(p > 2.7e-8 && p < 2.9e-8, "price was {p}");
    }
}
