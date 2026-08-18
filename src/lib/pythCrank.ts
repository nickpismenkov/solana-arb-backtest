// Port of src/pyth_crank.rs
//
// Self-crank instruction builders (step 4 of the pipeline): turn a parsed
// Hermes update (pythAccumulator) into the on-chain ix sequence that posts a
// fresh price to the SHARED sponsored PriceUpdateV2 feed marginfi reads.
//
// Derived from a REAL mainnet sponsored-feed crank (the marginfi/Kamino
// lesson) — setup tx 2hkWPh62... + fire tx 4vYXYqdN..., same buffer keypair:
//
//   SETUP tx: system createAccount(space = 46 + vaa_len, owner = Wormhole-verify)
//             . init_encoded_vaa . write_encoded_vaa(index 0, first ~720B)
//   FIRE  tx: write_encoded_vaa(tail) . verify_encoded_vaa_v1(guardian_set)
//             . push-wrapper update_price_feed{PostUpdateParams, shard, feed_id}
//               (CPIs receiver post_update -> writes the sponsored feed PDA,
//                whose write_authority is itself — the permissionless pattern)
//             . close_encoded_vaa (rent back)
//
// The two-tx split exists because the guardian-signed VAA (~952B for one
// Hermes blob) can't fit a single 1232B tx next to the 396B update ix. One
// encoded-VAA buffer can serve several update ixs (the VAA commits to the
// merkle root of ALL feeds in the blob); only the last fire tx should close.
//
// Observed rent for space=998 was 7,836,960 lamports = (space+128)*6960 —
// reclaimed by close_encoded_vaa.

import {
  Keypair,
  PublicKey,
  TransactionInstruction,
  TransactionMessage,
  VersionedTransaction,
} from '@solana/web3.js';
import type { MerkleUpdate } from './pythAccumulator.js';

/** Wormhole verification program the Pyth receiver trusts (encoded-VAA flow). */
export const WORMHOLE_VERIFY = 'HDwcJBJXjL9FpJ7UBsYBtaDjsBUhuLCUYoz3zr8SWWaQ';
/** Pyth pull-oracle receiver (post_update lives here; wrapper CPIs into it). */
export const PYTH_RECEIVER = 'rec5EKMGg6MxZYaMdyBfgwp4d5rB9T1VQH5pJv5LtFJ';
/** Pyth push wrapper — the ONLY writer of the shared sponsored feeds. */
export const PUSH_ORACLE = 'pythWSnswVUd12oZpeFP8e9CVaEqJg25g1Vtc2biRsT';
/** Receiver config PDA (seed "config"), pinned from the captured tx. */
export const RECEIVER_CONFIG = 'DaWUKXCyXsnzcvLUyeJRWou8KTn7XtadgTsdhJ6RHS7b';
const SYSTEM = '11111111111111111111111111111111';

// VERIFIED discriminators (sha256("global:<name>")[..8], matched against the
// captured crank — see pyth_crank_decode).
const DISC_INIT_ENCODED_VAA = Buffer.from([0xd1, 0xc1, 0xad, 0x19, 0x5b, 0xca, 0xb5, 0xda]);
const DISC_WRITE_ENCODED_VAA = Buffer.from([0xc7, 0xd0, 0x6e, 0xb1, 0x96, 0x4c, 0x76, 0x2a]);
const DISC_VERIFY_ENCODED_VAA_V1 = Buffer.from([0x67, 0x38, 0xb1, 0xe5, 0xf0, 0x67, 0x44, 0x49]);
const DISC_CLOSE_ENCODED_VAA = Buffer.from([0x30, 0xdd, 0xae, 0xc6, 0xe7, 0x07, 0x98, 0x26]);
const DISC_UPDATE_PRICE_FEED = Buffer.from([0x1c, 0x09, 0x5d, 0x96, 0x56, 0x99, 0xbc, 0x73]);

/**
 * First-chunk size for the setup tx (observed cranker used ~720; the tail +
 * verify + update + close must fit the 1232B fire tx).
 */
export const SETUP_CHUNK = 720;

function pk(s: string): PublicKey {
  return new PublicKey(s);
}

/**
 * EncodedVaa account size: 46B header (disc + status + write_authority +
 * version + len) + the raw VAA. Observed: 952B VAA -> space 998.
 */
export function encodedVaaSpace(vaaLen: number): number {
  return 46 + vaaLen;
}

/** Rent-exempt minimum, matching the observed createAccount exactly. */
export function rentLamports(space: number): number {
  return (space + 128) * 6960;
}

/** Guardian-set index a VAA was signed under (u32 BE right after the version). */
export function guardianSetIndex(vaa: Buffer): number | undefined {
  if (vaa.length < 5) return undefined;
  return vaa.readUInt32BE(1);
}

/** Guardian-set PDA of the Wormhole-verify program. */
export function guardianSet(index: number): PublicKey {
  const idxBuf = Buffer.alloc(4);
  idxBuf.writeUInt32BE(index, 0);
  const [addr] = PublicKey.findProgramAddressSync([Buffer.from('GuardianSet'), idxBuf], pk(WORMHOLE_VERIFY));
  return addr;
}

/** Receiver treasury PDA (any id 0..=255 works; ids spread write contention). */
export function treasury(id: number): PublicKey {
  const [addr] = PublicKey.findProgramAddressSync([Buffer.from('treasury'), Buffer.from([id])], pk(PYTH_RECEIVER));
  return addr;
}

/** Sponsored-feed PDA the push wrapper writes: seeds [shard_le, feed_id]. */
export function sponsoredFeed(shard: number, feedId: Buffer): PublicKey {
  const shardBuf = Buffer.alloc(2);
  shardBuf.writeUInt16LE(shard, 0);
  const [addr] = PublicKey.findProgramAddressSync([shardBuf, feedId], pk(PUSH_ORACLE));
  return addr;
}

/** System createAccount for the encoded-VAA buffer (buffer must co-sign). */
export function createEncodedVaaAccountIx(payer: PublicKey, buffer: PublicKey, vaaLen: number): TransactionInstruction {
  const space = encodedVaaSpace(vaaLen);
  const data = Buffer.alloc(52);
  let o = 0;
  data.writeUInt32LE(0, o); o += 4; // CreateAccount tag
  data.writeBigUInt64LE(BigInt(rentLamports(space)), o); o += 8;
  data.writeBigUInt64LE(BigInt(space), o); o += 8;
  pk(WORMHOLE_VERIFY).toBuffer().copy(data, o); o += 32;
  return new TransactionInstruction({
    programId: pk(SYSTEM),
    keys: [
      { pubkey: payer, isSigner: true, isWritable: true },
      { pubkey: buffer, isSigner: true, isWritable: true },
    ],
    data: data.subarray(0, o),
  });
}

export function initEncodedVaaIx(writeAuthority: PublicKey, buffer: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(WORMHOLE_VERIFY),
    keys: [
      { pubkey: writeAuthority, isSigner: true, isWritable: false },
      { pubkey: buffer, isSigner: false, isWritable: true },
    ],
    data: DISC_INIT_ENCODED_VAA,
  });
}

/** write_encoded_vaa { index: u32 (byte offset), data: Vec<u8> }. */
export function writeEncodedVaaIx(
  writeAuthority: PublicKey,
  buffer: PublicKey,
  index: number,
  chunk: Buffer,
): TransactionInstruction {
  const data = Buffer.alloc(16 + chunk.length);
  let o = 0;
  DISC_WRITE_ENCODED_VAA.copy(data, o); o += 8;
  data.writeUInt32LE(index, o); o += 4;
  data.writeUInt32LE(chunk.length, o); o += 4;
  chunk.copy(data, o); o += chunk.length;
  return new TransactionInstruction({
    programId: pk(WORMHOLE_VERIFY),
    keys: [
      { pubkey: writeAuthority, isSigner: true, isWritable: false },
      { pubkey: buffer, isSigner: false, isWritable: true },
    ],
    data: data.subarray(0, o),
  });
}

export function verifyEncodedVaaV1Ix(
  writeAuthority: PublicKey,
  buffer: PublicKey,
  guardianSetPk: PublicKey,
): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(WORMHOLE_VERIFY),
    keys: [
      { pubkey: writeAuthority, isSigner: true, isWritable: false },
      { pubkey: buffer, isSigner: false, isWritable: true },
      { pubkey: guardianSetPk, isSigner: false, isWritable: false },
    ],
    data: DISC_VERIFY_ENCODED_VAA_V1,
  });
}

/** Rent goes back to the write authority. */
export function closeEncodedVaaIx(writeAuthority: PublicKey, buffer: PublicKey): TransactionInstruction {
  return new TransactionInstruction({
    programId: pk(WORMHOLE_VERIFY),
    keys: [
      { pubkey: writeAuthority, isSigner: true, isWritable: true },
      { pubkey: buffer, isSigner: false, isWritable: true },
    ],
    data: DISC_CLOSE_ENCODED_VAA,
  });
}

/**
 * Push-wrapper update_price_feed(PostUpdateParams { merkle_price_update
 * { message: Vec<u8>, proof: Vec<[u8;20]> }, treasury_id: u8 }, shard: u16,
 * feed_id: [u8;32]) — 396B observed for an 85B message + 13-hash proof.
 * Converts the accumulator's wire proof (u8 count + count*20B) to borsh
 * (u32 count + hashes). Returns undefined on a malformed proof.
 */
export function updatePriceFeedIx(
  payer: PublicKey,
  encodedVaa: PublicKey,
  update: MerkleUpdate,
  shard: number,
  treasuryId: number,
): TransactionInstruction | undefined {
  const feedId = update.feedId();
  if (feedId === undefined) return undefined;
  if (update.proof.length < 1) return undefined;
  const n = update.proof[0];
  if (update.proof.length < 1 + n * 20) return undefined;
  const hashes = update.proof.subarray(1, 1 + n * 20);

  const data = Buffer.alloc(8 + 4 + update.message.length + 4 + hashes.length + 35);
  let o = 0;
  DISC_UPDATE_PRICE_FEED.copy(data, o); o += 8;
  data.writeUInt32LE(update.message.length, o); o += 4;
  update.message.copy(data, o); o += update.message.length;
  data.writeUInt32LE(n, o); o += 4;
  hashes.copy(data, o); o += hashes.length;
  data.writeUInt8(treasuryId, o); o += 1;
  data.writeUInt16LE(shard, o); o += 2;
  feedId.copy(data, o); o += 32;

  return new TransactionInstruction({
    programId: pk(PUSH_ORACLE),
    keys: [
      { pubkey: payer, isSigner: true, isWritable: true },
      { pubkey: pk(PYTH_RECEIVER), isSigner: false, isWritable: false },
      { pubkey: encodedVaa, isSigner: false, isWritable: false },
      { pubkey: pk(RECEIVER_CONFIG), isSigner: false, isWritable: false },
      { pubkey: treasury(treasuryId), isSigner: false, isWritable: true },
      { pubkey: sponsoredFeed(shard, feedId), isSigner: false, isWritable: true },
      { pubkey: pk(SYSTEM), isSigner: false, isWritable: false },
    ],
    data: data.subarray(0, o),
  });
}

/**
 * The full two-tx crank for one Hermes blob: setup (create+init+first chunk)
 * and fire (tail+verify+update per feed+close). `updates` may hold several
 * feeds — one VAA proves them all; each gets its own update ix in the fire tx
 * if it fits (each adds ~430B; more than 2 feeds needs splitting upstream).
 */
export interface CrankIxs {
  setup: TransactionInstruction[];
  fire: TransactionInstruction[];
}

export function buildCrankIxs(
  payer: PublicKey,
  buffer: PublicKey,
  vaa: Buffer,
  updates: MerkleUpdate[],
  shard: number,
  treasuryId: number,
): CrankIxs | undefined {
  const gsIndex = guardianSetIndex(vaa);
  if (gsIndex === undefined) return undefined;
  const gs = guardianSet(gsIndex);
  const head = vaa.subarray(0, Math.min(SETUP_CHUNK, vaa.length));
  const setup = [
    createEncodedVaaAccountIx(payer, buffer, vaa.length),
    initEncodedVaaIx(payer, buffer),
    writeEncodedVaaIx(payer, buffer, 0, head),
  ];
  const fire: TransactionInstruction[] = [];
  if (vaa.length > head.length) {
    fire.push(writeEncodedVaaIx(payer, buffer, head.length, vaa.subarray(head.length)));
  }
  fire.push(verifyEncodedVaaV1Ix(payer, buffer, gs));
  for (const u of updates) {
    const ix = updatePriceFeedIx(payer, buffer, u, shard, treasuryId);
    if (ix === undefined) return undefined;
    fire.push(ix);
  }
  fire.push(closeEncodedVaaIx(payer, buffer));
  return { setup, fire };
}

// -- Transaction-level assembly (what the executor bundles ahead of the
//    liquidate tx) --------------------------------------------------------

/**
 * CU ceilings from the captured crank: setup ran in ~5.4k, fire in ~402k
 * (verify_encoded_vaa_v1 does 13 secp recoveries).
 */
export const SETUP_CU = 30_000;
export const FIRE_CU = 500_000;

function cuLimitIx(units: number): TransactionInstruction {
  const data = Buffer.alloc(5);
  data.writeUInt8(2, 0);
  data.writeUInt32LE(units, 1);
  return new TransactionInstruction({
    programId: pk('ComputeBudget111111111111111111111111111111'),
    keys: [],
    data,
  });
}

/**
 * The two crank txs plus the ephemeral buffer keypair that must co-sign the
 * setup (createAccount). Build fresh per fire — the VAA inside is only as
 * fresh as the Hermes blob it came from.
 */
export class CrankTxs {
  setup: VersionedTransaction;
  fire: VersionedTransaction;
  buffer: Keypair;

  constructor(setup: VersionedTransaction, fire: VersionedTransaction, buffer: Keypair) {
    this.setup = setup;
    this.fire = fire;
    this.buffer = buffer;
  }

  /** Stamp a recent blockhash and sign: setup = [payer, buffer], fire = [payer]. */
  stampAndSign(payer: Keypair, blockhash: string): void {
    (this.setup.message as any).recentBlockhash = blockhash;
    (this.fire.message as any).recentBlockhash = blockhash;
    this.setup.sign([payer, this.buffer]);
    this.fire.sign([payer]);
  }

  /** (setup, fire) as base64 — the encoding sendBundle/simulateBundle take. */
  toB64(): [string, string] {
    const e = (tx: VersionedTransaction): string => Buffer.from(tx.serialize()).toString('base64');
    return [e(this.setup), e(this.fire)];
  }
}

/**
 * Compile the two-tx crank for `updates` (usually one feed) with placeholder
 * signatures. `blockhash` may be default for replace-blockhash simulation;
 * stampAndSign before a live send.
 */
export function buildCrankTxs(
  payer: PublicKey,
  vaa: Buffer,
  updates: MerkleUpdate[],
  shard: number,
  treasuryId: number,
  blockhash: string,
): CrankTxs {
  const buffer = Keypair.generate();
  const ixs = buildCrankIxs(payer, buffer.publicKey, vaa, updates, shard, treasuryId);
  if (ixs === undefined) throw new Error('crank ix build failed (malformed VAA/update)');

  const compile = (cu: number, body: TransactionInstruction[]): VersionedTransaction => {
    const all = [cuLimitIx(cu), ...body];
    const msg = new TransactionMessage({
      payerKey: payer,
      recentBlockhash: blockhash,
      instructions: all,
    }).compileToV0Message([]);
    return new VersionedTransaction(msg);
  };

  return new CrankTxs(compile(SETUP_CU, ixs.setup), compile(FIRE_CU, ixs.fire), buffer);
}
