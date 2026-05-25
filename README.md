# sub2api-facilitator

Self-hosted x402 facilitator for Sub2API/AI.GG.

This service is built on the official `github.com/x402-foundation/x402/go` facilitator SDK and
registers the official EVM `exact` scheme for one configured network.

## Endpoints

- `GET /health`
- `GET /supported`
- `POST /verify`
- `POST /settle`
- `GET /v1/x402/supported`
- `POST /v1/x402/verify`
- `POST /v1/x402/settle`

The `/v1/x402/*` routes are kept for Sub2API's existing facilitator URL integration.

## Configuration

```bash
LISTEN_ADDR=:18081
RPC_URL=https://mainnet.base.org
CHAIN_ID=8453
X402_NETWORK=eip155:8453
FACILITATOR_PRIVATE_KEY=0x...
CONFIRM_TIMEOUT_SECONDS=120
VERIFY_TIMEOUT_SECONDS=30
SETTLE_TIMEOUT_SECONDS=120
DEPLOY_ERC4337_WITH_EIP6492=true
SIMULATE_IN_SETTLE=false
```

`SEPOLIA_RPC_URL` is still accepted as a backward-compatible alias for `RPC_URL` because the first
prototype used that variable name even when pointed at Base.

## Supported Assets

The official EVM exact scheme settles EIP-3009 `transferWithAuthorization` payments. Supported
tokens must implement:

- `transferWithAuthorization`
- `authorizationState`

Base production assets currently used by AI.GG:

- USDC: `0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913`
- GCT: `0x7CCb0D3F16C9Ea94a189E14C1d92f6561D707fa4`

The asset address, token name, and token version are supplied by the Sub2API payment requirement
payload, so the facilitator does not maintain a separate asset whitelist.
