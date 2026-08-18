// Port of src/pools.rs
//
// Pool pair config + on-chain price decode. Byte offsets verified against
// mainnet (see git history / RUN.md). Same Q64.64 sqrt-price math for both
// venues — Orca Whirlpool and Raydium CLMM.
//
// The pair is env-configurable so the same tooling re-points at any pair that
// has a pool on both venues (defaults = liquid SOL/USDC). Env:
//   PAIR_LABEL, ORCA_POOL, RAY_CLMM_POOL, BASE_MINT, BASE_DECIMALS,
//   QUOTE_DECIMALS, ORCA_FEE_BPS, RAY_FEE_BPS, RAY_VAULT0

import bs58 from 'bs58';

const TOKEN_2022_PROGRAM = 'TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb';

export interface PairCfg {
  label: string;
  orcaPool: string;
  rayPool: string;
  /** Orientation: prices are quote-per-base on both venues. */
  baseMint: string;
  /** The Whirlpool account doesn't carry decimals (Ray CLMM does). */
  baseDec: number;
  quoteDec: number;
  orcaFeeBps: number;
  rayFeeBps: number;
  /** Ray CLMM token_vault_0 — input vault == this ⇒ base is being sold. */
  rayVault0: string;
  /**
   * Token program owning each mint (classic SPL Token or Token-2022). A
   * Token-2022 leg requires swapV2 + Token-2022 ATAs. Default: both classic.
   */
  baseTokenProgram: string;
  quoteTokenProgram: string;
  /** True if either side is Token-2022 → must use swapV2 + Token-2022 ATAs. */
  needsSwapV2(): boolean;
  roundTripFeeBps(): number;
}

function envOr(key: string, def: string): string {
  return process.env[key] ?? def;
}

function envNum(key: string, def: number): number {
  const v = process.env[key];
  if (v === undefined) return def;
  const n = Number(v);
  return Number.isFinite(n) ? n : def;
}

function makeCfg(): PairCfg {
  return {
    label: envOr('PAIR_LABEL', 'SOL/USDC'),
    orcaPool: envOr('ORCA_POOL', 'Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE'),
    // Default Raydium CLMM SOL/USDC — the active 4bp pool; one account
    // holds sqrtPrice so it updates on every swap (no vault-ratio lag).
    rayPool: envOr('RAY_CLMM_POOL', '3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv'),
    baseMint: envOr('BASE_MINT', 'So11111111111111111111111111111111111111112'),
    baseDec: envNum('BASE_DECIMALS', 9),
    quoteDec: envNum('QUOTE_DECIMALS', 6),
    orcaFeeBps: envNum('ORCA_FEE_BPS', 4.0),
    rayFeeBps: envNum('RAY_FEE_BPS', 4.0),
    rayVault0: envOr('RAY_VAULT0', '4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A'),
    baseTokenProgram: envOr('BASE_TOKEN_PROGRAM', 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA'),
    quoteTokenProgram: envOr('QUOTE_TOKEN_PROGRAM', 'TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA'),
    needsSwapV2(): boolean {
      return this.baseTokenProgram === TOKEN_2022_PROGRAM || this.quoteTokenProgram === TOKEN_2022_PROGRAM;
    },
    roundTripFeeBps(): number {
      return this.orcaFeeBps + this.rayFeeBps;
    },
  };
}

/** Lazily loaded pair config. Call after dotenv has run (all bins do). */
let CFG: PairCfg | undefined;
export function pair(): PairCfg {
  if (CFG === undefined) CFG = makeCfg();
  return CFG;
}

function u128LE(d: Uint8Array, o: number): bigint {
  const view = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const lo = view.getBigUint64(o, true);
  const hi = view.getBigUint64(o + 8, true);
  return (hi << 64n) | lo;
}

function mintAt(d: Uint8Array, o: number): string {
  return bs58.encode(d.subarray(o, o + 32));
}

/** Orca Whirlpool: sqrtPrice (Q64.64) @65, mintA @101, mintB @181. */
export function orcaPrice(d: Uint8Array): number | undefined {
  if (d.length < 213) return undefined;
  const cfg = pair();
  const sqrt = Number(u128LE(d, 65)) / 2 ** 64;
  const mintA = mintAt(d, 101);
  const baseIsA = mintA === cfg.baseMint;
  const [decA, decB] = baseIsA ? [cfg.baseDec, cfg.quoteDec] : [cfg.quoteDec, cfg.baseDec];
  const uiBPerA = sqrt * sqrt * 10 ** (decA - decB);
  return baseIsA ? uiBPerA : 1.0 / uiBPerA;
}

/**
 * Raydium CLMM PoolState: mint0 @73, mint1 @105, decimals @233/234,
 * sqrtPriceX64 @253.
 */
export function rayClmmPrice(d: Uint8Array): number | undefined {
  if (d.length < 269) return undefined;
  const mint0 = mintAt(d, 73);
  const dec0 = d[233];
  const dec1 = d[234];
  const sqrt = Number(u128LE(d, 253)) / 2 ** 64;
  const uiT1PerT0 = sqrt * sqrt * 10 ** (dec0 - dec1);
  return mint0 === pair().baseMint ? uiT1PerT0 : 1.0 / uiT1PerT0;
}
