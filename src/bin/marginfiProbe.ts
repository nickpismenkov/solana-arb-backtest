// Port of src/bin/marginfi_probe.rs
//
// marginfi flash-loan GO/NO-GO. Does an empty marginfi flashloan (borrow 1
// USDC → deposit it straight back → end, net-zero balances) LAND in a Jito
// bundle — the thing Jupiter Lend can't do? One-time: creates a MarginfiAccount
// (plain keypair, persisted). Always simulates first; LIVE=1 submits.
// MODE=jito (default, the real test) or MODE=rpc (control — proves the tx is
// valid regardless of Jito).
//
// Usage: RPC_ENDPOINT=<url> KEYPAIR_PATH=<path> \
//   MARGINFI_ACCOUNT_KEYPAIR=<path, created if absent> \
//   [LIVE=1] [MODE=jito|rpc] [TIP_LAMPORTS=1000000] \
//   [MARGINFI_USDC_VAULT=<pk> MARGINFI_USDC_VAULT_AUTH=<pk>] [MARGINFI_DEPOSIT_OPT=1] \
//   npx tsx src/bin/marginfiProbe.ts

import 'dotenv/config';
import { readFileSync, writeFileSync } from 'node:fs';
import bs58 from 'bs58';
import { Keypair, PublicKey, TransactionMessage, VersionedTransaction } from '@solana/web3.js';
import { cuLimitIx, cuPriceIx, transferIx } from '../lib/arb.js';
import { ata, createAtaIdempotent } from '../lib/flashloan.js';
import { defaultBlockEngine, getTipAccounts, sendBundle } from '../lib/jito.js';
import { accountInitialize, borrowUsdc, endFlashloan, paybackUsdc, startFlashloan, usdcVault, usdcVaultAuthority, USDC_MINT } from '../lib/marginfi.js';

async function rpc(endpoint: string, body: unknown): Promise<any | null> {
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const resp = await fetch(endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return await resp.json();
    } catch {
      /* retry */
    }
    await new Promise((r) => setTimeout(r, 300 << attempt));
  }
  return null;
}

async function finalizedBlockhash(endpoint: string): Promise<string> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getLatestBlockhash',
    params: [{ commitment: 'finalized' }],
  });
  const bh = v?.result?.value?.blockhash;
  if (typeof bh !== 'string') throw new Error('blockhash');
  return bh;
}

async function accountExists(endpoint: string, pk: PublicKey): Promise<boolean> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getAccountInfo',
    params: [pk.toBase58(), { encoding: 'base64' }],
  });
  if (v === null) return false;
  return v?.result?.value !== null && v?.result?.value !== undefined;
}

async function landed(endpoint: string, sig: string): Promise<any | null> {
  const v = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'getTransaction',
    params: [sig, { encoding: 'json', maxSupportedTransactionVersion: 0, commitment: 'confirmed' }],
  });
  if (v === null) return null;
  const result = v?.result;
  return result === null || result === undefined ? null : result;
}

function loadOrMakeKeypair(path: string): [Keypair, boolean] {
  try {
    const s = readFileSync(path, 'utf8');
    const bytes: number[] = JSON.parse(s);
    return [Keypair.fromSecretKey(Uint8Array.from(bytes)), false];
  } catch {
    /* fall through to generate */
  }
  const kp = Keypair.generate();
  writeFileSync(path, JSON.stringify([...kp.secretKey]));
  return [kp, true];
}

function sign(msg: ReturnType<TransactionMessage['compileToV0Message']>, signers: Keypair[]): VersionedTransaction {
  const tx = new VersionedTransaction(msg);
  tx.sign(signers);
  return tx;
}

async function main(): Promise<void> {
  const endpoint = process.env.RPC_ENDPOINT;
  if (!endpoint) throw new Error('RPC_ENDPOINT');
  const keypairPath = process.env.KEYPAIR_PATH;
  if (!keypairPath) throw new Error('KEYPAIR_PATH');
  // Either a keypair path (to create/own the account) OR just the pubkey
  // (MARGINFI_ACCOUNT) for read-only flows like MODE=simbundle — the flashloan
  // tx doesn't need the marginfi account to sign, only the authority.
  let mfiAccOverride: PublicKey | undefined;
  if (process.env.MARGINFI_ACCOUNT) {
    try {
      mfiAccOverride = new PublicKey(process.env.MARGINFI_ACCOUNT);
    } catch {
      mfiAccOverride = undefined;
    }
  }
  const mfiAccPath = process.env.MARGINFI_ACCOUNT_KEYPAIR ?? '';
  const live = process.env.LIVE === '1';
  const mode = process.env.MODE ?? 'jito';
  const tipLamports = BigInt(process.env.TIP_LAMPORTS ?? '1000000');
  const blockEngine = defaultBlockEngine();

  const bytes: number[] = JSON.parse(readFileSync(keypairPath, 'utf8'));
  const authority = Keypair.fromSecretKey(Uint8Array.from(bytes));
  const signer = authority.publicKey;
  const usdc = new PublicKey(USDC_MINT);
  const usdcAta = ata(signer, usdc);

  console.log(`authority=${signer.toBase58()}`);
  console.log(`usdc vault=${usdcVault().toBase58()} auth=${usdcVaultAuthority().toBase58()}`);

  // Resolve marginfi account: pubkey override (read-only) or keypair (owner).
  let mfiAcc: PublicKey;
  let mfiAccKp: Keypair | undefined;
  if (mfiAccOverride) {
    mfiAcc = mfiAccOverride;
    console.log(`marginfi account=${mfiAcc.toBase58()} (pubkey override — read-only)`);
  } else {
    if (mfiAccPath === '') {
      throw new Error('set MARGINFI_ACCOUNT=<pubkey> or MARGINFI_ACCOUNT_KEYPAIR=<path>');
    }
    const [kp, freshlyMade] = loadOrMakeKeypair(mfiAccPath);
    console.log(`marginfi account=${kp.publicKey.toBase58()}${freshlyMade ? ' (NEW keypair generated)' : ''}`);
    mfiAcc = kp.publicKey;
    mfiAccKp = kp;
  }

  // ── one-time: create the MarginfiAccount on-chain ──
  if (!(await accountExists(endpoint, mfiAcc))) {
    if (!mfiAccKp) {
      console.log(`account ${mfiAcc.toBase58()} doesn't exist and no keypair to create it (pubkey-override mode)`);
      return;
    }
    if (!live) {
      console.log('marginfi account does not exist yet — rerun with LIVE=1 to create it (one-time, ~0.016 SOL rent)');
      return;
    }
    console.log('creating MarginfiAccount…');
    const bh = await finalizedBlockhash(endpoint);
    const ixs = [cuLimitIx(60_000), cuPriceIx(10_000n), accountInitialize(mfiAcc, signer, signer)];
    const msg = new TransactionMessage({ payerKey: signer, recentBlockhash: bh, instructions: ixs }).compileToV0Message([]);
    const tx = sign(msg, [authority, mfiAccKp]);
    const b64 = Buffer.from(tx.serialize()).toString('base64');
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'sendTransaction',
      params: [b64, { encoding: 'base64', skipPreflight: false, preflightCommitment: 'confirmed', maxRetries: 5 }],
    });
    if (v === null) throw new Error('send init');
    if (v?.error != null) {
      console.log(`⛔ MarginfiAccount init rejected: ${JSON.stringify(v.error)}`);
      process.exit(1);
    }
    const sig: string = typeof v?.result === 'string' ? v.result : '';
    console.log(`  init sig ${sig} — waiting for confirmation…`);
    let ok = false;
    for (let i = 0; i < 20; i++) {
      await new Promise((r) => setTimeout(r, 3000));
      const meta = await landed(endpoint, sig);
      if (meta !== null) {
        console.log(`  ✅ MarginfiAccount created (slot ${meta?.slot}, err ${JSON.stringify(meta?.meta?.err ?? null)})`);
        ok = true;
        break;
      }
    }
    if (!ok) {
      console.log(`⚠️ init not confirmed after 60s — check ${sig} and rerun (keypair saved at ${mfiAccPath})`);
      return;
    }
  } else {
    console.log('MarginfiAccount already exists — reusing');
  }

  // ── the flashloan test tx ──
  // ix layout (end_index = 6): 0 cu_limit, 1 cu_price, 2 create-ATA,
  // 3 start_flashloan(6), 4 borrow 1 USDC, 5 deposit 1 USDC, 6 end_flashloan, 7 tip.
  let tipTo: PublicKey | undefined;
  for (let i = 0; i < 12; i++) {
    try {
      const v = await getTipAccounts(blockEngine);
      if (v.length > 0) {
        tipTo = v[0];
        break;
      }
    } catch {
      /* retry below */
    }
    await new Promise((r) => setTimeout(r, 3000));
  }
  if (!tipTo) throw new Error('tip accounts (rate limited)');
  const bh = await finalizedBlockhash(endpoint);
  console.log(`blockhash ${bh}`);
  const ixs = [
    cuLimitIx(400_000),
    cuPriceIx(10_000n),
    createAtaIdempotent(signer, usdc),
    startFlashloan(mfiAcc, signer, 6n),
    borrowUsdc(mfiAcc, signer, usdcAta, 1_000_000n),
    paybackUsdc(mfiAcc, signer, usdcAta, 1_000_000n, true),
    endFlashloan(mfiAcc, signer, []), // net-zero → empty remaining
    transferIx(signer, tipTo, tipLamports),
  ];
  const msg = new TransactionMessage({ payerKey: signer, recentBlockhash: bh, instructions: ixs }).compileToV0Message([]);
  const tx = sign(msg, [authority]);
  const sig = bs58.encode(tx.signatures[0]);
  const raw = tx.serialize();
  const b64 = Buffer.from(raw).toString('base64');
  console.log(`marginfi flashloan tx ${raw.length}B sig=${sig} tip=${tipLamports}`);

  // simulate first
  const sim = await rpc(endpoint, {
    jsonrpc: '2.0',
    id: 1,
    method: 'simulateTransaction',
    params: [b64, { encoding: 'base64', sigVerify: false, replaceRecentBlockhash: true }],
  });
  if (sim === null) throw new Error('simulate');
  const err = sim?.result?.value?.err;
  if (err !== null && err !== undefined) {
    console.log(`⛔ simulation FAILED: ${JSON.stringify(err)}`);
    const logs: any[] = Array.isArray(sim?.result?.value?.logs) ? sim.result.value.logs : [];
    for (const l of logs) console.log(`  ${typeof l === 'string' ? l : ''}`);
    console.log('\n(fix accounts/args from the logs above — likely vault PDA or deposit arg; see env overrides)');
    process.exit(1);
  }
  console.log(`✅ simulates clean (${sim?.result?.value?.unitsConsumed} CU)`);

  // MODE=simbundle: run Jito's simulateBundle — executes the bundle exactly as
  // the block engine would (needs a simulateBundle-capable RPC, e.g. Helius).
  // Read-only, no cost. Rules out a filter/execution problem: if this succeeds
  // but the live bundle never lands, the barrier is the AUCTION (tip/profit),
  // not Jito rejecting the bundle.
  if (mode === 'simbundle') {
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'simulateBundle',
      params: [{ encodedTransactions: [b64] }],
    });
    if (v !== null && v?.error != null) {
      console.log(`simulateBundle error: ${JSON.stringify(v.error)}`);
    } else if (v !== null) {
      const val = v?.result?.value;
      console.log(`simulateBundle summary: ${JSON.stringify(val?.summary)}`);
      const results: any[] = Array.isArray(val?.transactionResults) ? val.transactionResults : [];
      results.forEach((r, i) => {
        console.log(`  tx[${i}] err=${JSON.stringify(r?.err)} cu=${r?.unitsConsumed}`);
        const rlogs: any[] = Array.isArray(r?.logs) ? r.logs : [];
        const tail = rlogs.slice(Math.max(0, rlogs.length - 3));
        for (const l of tail) console.log(`    ${typeof l === 'string' ? l : ''}`);
      });
    } else {
      console.log('simulateBundle: no response (does this RPC support it? use Helius)');
    }
    return;
  }

  if (!live) {
    console.log(`dry run — rerun with LIVE=1 to submit via ${mode} (~${tipLamports + 10_000n} lamports if it lands)`);
    return;
  }

  // submit
  if (mode === 'rpc') {
    const v = await rpc(endpoint, {
      jsonrpc: '2.0',
      id: 1,
      method: 'sendTransaction',
      params: [b64, { encoding: 'base64', skipPreflight: false, preflightCommitment: 'confirmed', maxRetries: 5 }],
    });
    if (v === null) throw new Error('send');
    if (v?.error != null) {
      console.log(`⛔ rpc rejected: ${JSON.stringify(v.error)}`);
      process.exit(1);
    }
    console.log(`⚡ sent via plain RPC: ${v?.result}`);
  } else {
    let attempt = 0;
    let id: string;
    for (;;) {
      attempt += 1;
      try {
        id = await sendBundle(blockEngine, [b64]);
        break;
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        if (msg.includes('429') && attempt < 12) {
          console.log(`  [attempt ${attempt}] rate limited, retry in 5s…`);
          await new Promise((r) => setTimeout(r, 5000));
          continue;
        }
        throw new Error(`send bundle: ${msg}`);
      }
    }
    console.log(`⚡ submitted Jito bundle ${id} (attempt ${attempt})`);
  }

  for (let i = 1; i <= 18; i++) {
    await new Promise((r) => setTimeout(r, 5000));
    const meta = await landed(endpoint, sig);
    if (meta !== null) {
      console.log(`\n🎉 LANDED via ${mode} — slot ${meta?.slot} fee ${meta?.meta?.fee} err ${JSON.stringify(meta?.meta?.err ?? null)}`);
      console.log(`https://solscan.io/tx/${sig}`);
      return;
    }
    console.log(`[${i * 5}s] on_chain=false`);
  }
  console.log(
    `\n⚠️ not landed after 90s via ${mode}. If MODE=rpc landed but jito didn't → flash loans are filtered on Jito generally → pivot to inventory mode.`,
  );
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
