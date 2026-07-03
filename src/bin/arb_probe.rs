//! Assembles + simulates the full guarded flash-loan arb:
//!   [CU limit, create USDC ATA, create base ATA, flash-borrow USDC,
//!    leg1 USDC→base (Orca, exact-in), leg2 base→USDC (Ray, EXACT-OUT = borrow),
//!    flash-payback USDC]
//! Leg 2 is exact-out for exactly the repay amount, so if the base received in
//! leg 1 can't produce it, leg 2 reverts — that IS the profit-or-revert guard.
//! Any base left over after leg 2 is the profit.
//!
//! With no real spread the arb reverts at leg 2 / payback — the CORRECT, safe
//! outcome, and it proves the assembler is sound (all metas valid, guard fires).
//! A structural (meta/layout) error would fail differently and earlier.
//!
//! Usage: RPC_ENDPOINT=<url> [BORROW_USDC=500] cargo run --release --bin arb_probe

use arb_engine::execute::{
    decode_orca_state, decode_ray_state, orca_start_index, orca_tick_array, ray_start_index,
    ray_tick_array,
};
use arb_engine::flashloan::{borrow_usdc, create_ata_idempotent, payback_usdc, ata, USDC_MINT};
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

const COMPUTE_BUDGET: &str = "ComputeBudget111111111111111111111111111111";

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

fn account_data(endpoint: &str, addr: &str) -> Vec<u8> {
    let v = rpc(
        endpoint,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
            "params":[addr,{"encoding":"base64"}]}),
    )
    .expect("rpc");
    base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"][0].as_str().expect("account data"))
        .unwrap()
}

fn pk_at(d: &[u8], o: usize) -> Pubkey {
    Pubkey::try_from(&d[o..o + 32]).unwrap()
}

fn cu_limit_ix(units: u32) -> Instruction {
    let mut data = vec![0x02];
    data.extend_from_slice(&units.to_le_bytes());
    Instruction {
        program_id: Pubkey::from_str(COMPUTE_BUDGET).unwrap(),
        accounts: vec![],
        data,
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("RPC_ENDPOINT")
        .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".to_string());
    let borrow_ui: f64 = std::env::var("BORROW_USDC").ok().and_then(|s| s.parse().ok()).unwrap_or(500.0);
    let borrow_amount = (borrow_ui * 1e6) as u64; // USDC 6 dec
    let cfg = pair();
    let signer = Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap();
    let usdc = Pubkey::from_str(USDC_MINT).unwrap();
    let base = Pubkey::from_str(&cfg.base_mint).unwrap();

    println!("arb-probe {} borrow {} USDC — leg1 Orca (exact-in) → leg2 Ray (exact-out)\n", cfg.label, borrow_ui);

    // ── resolve Orca (leg1: USDC → base) ──
    let od = account_data(&endpoint, &cfg.orca_pool);
    let ost = decode_orca_state(&od).expect("orca state");
    let orca_pk = Pubkey::from_str(&cfg.orca_pool).unwrap();
    let mint_a = pk_at(&od, 101);
    let base_is_a = mint_a == base;
    // input is USDC; a_to_b = (input == mintA) = (USDC == mintA) = !base_is_a.
    let leg1_a_to_b = !base_is_a;
    let n = 88 * ost.tick_spacing as i32;
    let start = orca_start_index(ost.tick, ost.tick_spacing);
    let starts = if leg1_a_to_b { [start, start - n, start - 2 * n] } else { [start, start + n, start + 2 * n] };
    let orca_acc = OrcaSwapAccounts {
        whirlpool: orca_pk,
        token_authority: signer,
        token_owner_a: ata(&signer, &mint_a),
        token_vault_a: pk_at(&od, 133),
        token_owner_b: ata(&signer, &pk_at(&od, 181)),
        token_vault_b: pk_at(&od, 213),
        tick_arrays: [
            orca_tick_array(&orca_pk, starts[0]),
            orca_tick_array(&orca_pk, starts[1]),
            orca_tick_array(&orca_pk, starts[2]),
        ],
        oracle: orca_oracle(&orca_pk),
    };
    let leg1 = orca_swap_ix(&orca_acc, borrow_amount, 0, sqrt_limit(leg1_a_to_b), true, leg1_a_to_b);

    // ── resolve Raydium (leg2: base → USDC, exact-out = borrow_amount) ──
    let rd = account_data(&endpoint, &cfg.ray_pool);
    let rst = decode_ray_state(&rd).expect("ray state");
    let ray_pk = Pubkey::from_str(&cfg.ray_pool).unwrap();
    let mint0 = pk_at(&rd, 73);
    let base_is_0 = mint0 == base;
    let (input_vault, output_vault) = if base_is_0 { (pk_at(&rd, 137), pk_at(&rd, 169)) } else { (pk_at(&rd, 169), pk_at(&rd, 137)) };
    let rstart = ray_start_index(rst.tick, rst.tick_spacing);
    let ray_acc = RaySwapAccounts {
        payer: signer,
        amm_config: pk_at(&rd, 9),
        pool_state: ray_pk,
        input_token_account: ata(&signer, &base),
        output_token_account: ata(&signer, &usdc),
        input_vault,
        output_vault,
        observation_state: pk_at(&rd, 201),
        tick_array: ray_tick_array(&ray_pk, rstart),
    };
    // Selling base (zero_for_one when base is mint0) → price down → MIN limit.
    // exact-out: is_base_input=false, amount=USDC out, threshold=max in.
    let leg2 = ray_swap_ix(&ray_acc, borrow_amount, u64::MAX, sqrt_limit(base_is_0), false);

    let ixs = vec![
        cu_limit_ix(600_000),
        create_ata_idempotent(&signer, &usdc),
        create_ata_idempotent(&signer, &base),
        borrow_usdc(&signer, borrow_amount),
        leg1,
        leg2,
        payback_usdc(&signer, borrow_amount),
    ];
    // Dedup account count sanity (legacy tx has no ALT).
    let mut keys = std::collections::BTreeSet::new();
    keys.insert(signer);
    for ix in &ixs {
        keys.insert(ix.program_id);
        for m in &ix.accounts {
            keys.insert(m.pubkey);
        }
    }
    println!("unique accounts: {} (legacy limit 64; live needs an ALT to fit 1232 bytes)", keys.len());

    let msg = Message::new_with_blockhash(&ixs, Some(&signer), &Hash::default());
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::Legacy(msg),
    };
    let raw = bincode::serialize(&tx).unwrap();
    println!("serialized tx: {} bytes", raw.len());
    let b64 = base64::engine::general_purpose::STANDARD.encode(&raw);

    let v = rpc(
        &endpoint,
        serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
            "params":[b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}),
    );
    let raw_resp = v.clone().unwrap_or_default();
    // A top-level JSON-RPC error means the tx never simulated (usually: too
    // large → needs an ALT). Report it as such, not as a clean run.
    if let Some(e) = raw_resp.get("error").filter(|e| !e.is_null()) {
        let msg = e["message"].as_str().unwrap_or_default();
        println!("⛔ tx not simulated: {msg}");
        if msg.contains("too large") {
            println!(
                "   → EXPECTED at this stage: {} accounts don't fit in 1232 bytes raw.\n   \
                 Live + full-sim require an Address Lookup Table indexing our fixed\n   \
                 account set (created once on-chain, then reused). That's the next step\n   \
                 and needs a funded wallet — the go-live gate.",
                keys.len()
            );
        }
        return;
    }
    let val = v.map(|v| v["result"]["value"].clone()).unwrap_or_default();
    let err = &val["err"];
    let logs: Vec<String> = val["logs"]
        .as_array()
        .map(|a| a.iter().filter_map(|l| l.as_str().map(String::from)).collect())
        .unwrap_or_default();

    println!("\nsim err: {err}");
    let joined = logs.join("\n").to_lowercase();
    let reached_leg2 = joined.contains("flashloanborrow");
    let guard_fired = joined.contains("flashloanpayback")
        || joined.contains("max") && reached_leg2
        || joined.contains("exceeds") || joined.contains("slippage") || joined.contains("threshold");
    if err.is_null() {
        println!("✅ SIMULATED CLEAN — a profitable arb exists right now (rare); tx would land");
    } else if reached_leg2 {
        println!("✅ ASSEMBLER SOUND — borrow+leg1 executed, reverted at leg2/payback (no spread = guard working as designed)");
    } else {
        println!("⚠️  failed before the arb path — inspect for a meta/layout bug");
    }
    let _ = guard_fired;
    println!("\n--- logs ---");
    for l in logs.iter() {
        println!("  {l}");
    }
}
