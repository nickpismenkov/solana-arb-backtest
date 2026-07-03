//! Verifies the swap.rs instruction builders against mainnet by assembling a
//! real swap and simulating it. We don't need funds: Anchor validates the
//! account context (PDAs, owners, tick arrays, oracle) BEFORE the handler
//! runs, so a correct build fails late at the token transfer (unfunded ATA),
//! while a wrong meta fails early with a constraint/seeds/owner error. The
//! probe prints the error class + logs so we can tell which happened.
//!
//! Usage: RPC_ENDPOINT=<url> cargo run --release --bin swap_probe

use arb_engine::execute::{
    decode_orca_state, decode_ray_state, orca_start_index, orca_tick_array, ray_start_index,
    ray_tick_array,
};
use arb_engine::pools::pair;
use arb_engine::swap::{
    orca_oracle, orca_swap_ix, ray_swap_ix, sqrt_limit, OrcaSwapAccounts, RaySwapAccounts,
};
use base64::Engine;
use solana_hash::Hash;
use solana_instruction::Instruction;
use solana_message::{legacy::Message, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_signature::Signature;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

const ATA_PROGRAM: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
        }
        std::thread::sleep(std::time::Duration::from_millis(400 << attempt));
    }
    None
}

fn account_data(endpoint: &str, addr: &str) -> Option<Vec<u8>> {
    let v = rpc(
        endpoint,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
            "params":[addr,{"encoding":"base64"}]}),
    )?;
    base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"][0].as_str()?)
        .ok()
}

fn pk_at(d: &[u8], o: usize) -> Pubkey {
    Pubkey::try_from(&d[o..o + 32]).unwrap()
}

fn ata(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[
            owner.as_ref(),
            Pubkey::from_str(TOKEN_PROGRAM).unwrap().as_ref(),
            mint.as_ref(),
        ],
        &Pubkey::from_str(ATA_PROGRAM).unwrap(),
    )
    .0
}

fn simulate(endpoint: &str, ix: Instruction, authority: Pubkey) -> (Option<String>, Vec<String>) {
    let msg = Message::new_with_blockhash(&[ix], Some(&authority), &Hash::default());
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::Legacy(msg),
    };
    let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
    let v = rpc(
        endpoint,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}),
    );
    let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
    let err = if val["err"].is_null() {
        None
    } else {
        Some(val["err"].to_string())
    };
    let logs = val["logs"]
        .as_array()
        .map(|a| a.iter().filter_map(|l| l.as_str().map(String::from)).collect())
        .unwrap_or_default();
    (err, logs)
}

fn classify(err: &Option<String>, logs: &[String]) -> &'static str {
    match err {
        None => "✅ SIMULATED OK — metas correct, swap executed",
        Some(_) => {
            let joined = logs.join("\n").to_lowercase();
            // Failing on OUR token accounts (unfunded/uninitialized ATAs) means
            // every pool-side meta already validated — the builder is correct.
            if joined.contains("insufficient")
                || joined.contains("input_token_account")
                || joined.contains("output_token_account")
                || joined.contains("token_owner_account")
                || joined.contains("3012")
                || joined.contains("not be already initialized")
                || joined.contains("could not create program address")
                || joined.contains("account not found")
                || joined.contains("uninitialized")
            {
                "✅ METAS OK — reached swap handler; only our unfunded ATA failed"
            } else if joined.contains("seeds")
                || joined.contains("constraint")
                || joined.contains("owned by")
                || joined.contains("declared program")
            {
                "❌ METAS WRONG — account-context validation failed"
            } else {
                "⚠️  INCONCLUSIVE — inspect logs below"
            }
        }
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT")
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());
    let cfg = pair();
    // Any wallet works — we're checking account structure, not balances.
    let authority = Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap();

    // ── Orca ──
    println!("=== Orca {} pool {} ===", cfg.label, cfg.orca_pool);
    let od = account_data(&endpoint, &cfg.orca_pool).expect("orca pool");
    let ost = decode_orca_state(&od).expect("orca state");
    let orca_pk = Pubkey::from_str(&cfg.orca_pool).unwrap();
    let mint_a = pk_at(&od, 101);
    let mint_b = pk_at(&od, 181);
    let start = orca_start_index(ost.tick, ost.tick_spacing);
    // Three consecutive tick arrays in the swap direction (a_to_b = price down).
    let n = 88 * ost.tick_spacing as i32;
    let base_is_a = mint_a == Pubkey::from_str(&cfg.base_mint).unwrap();
    let a_to_b = base_is_a; // sell base = A→B when base is mintA
    let starts = if a_to_b {
        [start, start - n, start - 2 * n]
    } else {
        [start, start + n, start + 2 * n]
    };
    let oa = OrcaSwapAccounts {
        whirlpool: orca_pk,
        token_authority: authority,
        token_owner_a: ata(&authority, &mint_a),
        token_vault_a: pk_at(&od, 133),
        token_owner_b: ata(&authority, &mint_b),
        token_vault_b: pk_at(&od, 213),
        tick_arrays: [
            orca_tick_array(&orca_pk, starts[0]),
            orca_tick_array(&orca_pk, starts[1]),
            orca_tick_array(&orca_pk, starts[2]),
        ],
        oracle: orca_oracle(&orca_pk),
    };
    let ix = orca_swap_ix(&oa, 100_000, 0, sqrt_limit(a_to_b), a_to_b);
    let (err, logs) = simulate(&endpoint, ix, authority);
    println!("  a_to_b={a_to_b} err={err:?}");
    println!("  {}", classify(&err, &logs));
    for l in logs.iter().take(14) {
        println!("    {l}");
    }

    // ── Raydium CLMM ──
    println!("\n=== Raydium CLMM {} pool {} ===", cfg.label, cfg.ray_pool);
    let rd = account_data(&endpoint, &cfg.ray_pool).expect("ray pool");
    let rst = decode_ray_state(&rd).expect("ray state");
    let ray_pk = Pubkey::from_str(&cfg.ray_pool).unwrap();
    let amm_config = pk_at(&rd, 9);
    let mint0 = pk_at(&rd, 73);
    let mint1 = pk_at(&rd, 105);
    let vault0 = pk_at(&rd, 137);
    let vault1 = pk_at(&rd, 169);
    let observation = pk_at(&rd, 201);
    let base_is_0 = mint0 == Pubkey::from_str(&cfg.base_mint).unwrap();
    // Sell base: input is base. If base is mint0, input vault = vault0.
    let (input_mint, input_vault, output_vault) = if base_is_0 {
        (mint0, vault0, vault1)
    } else {
        (mint1, vault1, vault0)
    };
    let output_mint = if base_is_0 { mint1 } else { mint0 };
    let rstart = ray_start_index(rst.tick, rst.tick_spacing);
    let ra = RaySwapAccounts {
        payer: authority,
        amm_config,
        pool_state: ray_pk,
        input_token_account: ata(&authority, &input_mint),
        output_token_account: ata(&authority, &output_mint),
        input_vault,
        output_vault,
        observation_state: observation,
        tick_array: ray_tick_array(&ray_pk, rstart),
    };
    let is_base_input = true;
    let ix = ray_swap_ix(&ra, 100_000, 0, sqrt_limit(base_is_0), is_base_input);
    let (err, logs) = simulate(&endpoint, ix, authority);
    println!("  is_base_input={is_base_input} err={err:?}");
    println!("  {}", classify(&err, &logs));
    for l in logs.iter().take(14) {
        println!("    {l}");
    }
}
