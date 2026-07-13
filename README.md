# eth-explorer

A lightweight Etherscan-style REST API for the Ethereum Sepolia testnet,
built in Go with Gin, go-ethereum, Redis, and Viper/Zap.

> **Status:** work in progress, built milestone by milestone.
> Milestone 1 (this commit): project skeleton, configuration, logging,
> and a `GET /health` endpoint.

## Quick start (Milestone 1)

```bash
cp .env.example .env
# edit .env and set ETH_RPC_URL to a real Sepolia RPC endpoint
# (e.g. https://sepolia.infura.io/v3/<project-id>, or an Alchemy URL)

make tidy   # go mod tidy — resolves and downloads dependencies
make build  # go build ./cmd/server
make run    # starts the server on :8080 (or $SERVER_PORT)
```

Then:

```bash
curl http://localhost:8080/health
```

```json
{
  "success": true,
  "data": { "status": "ok", "version": "0.1.0" },
  "timestamp": "2026-07-13T12:00:00Z"
}
```

## Project layout

```
cmd/server/          application entrypoint (config → logger → router → server)
internal/api/
  handlers/           HTTP handlers (thin — parse input, call service, format output)
  routes/             route registration
  middleware/         logging, recovery, CORS, rate limiting (Milestone 2+)
internal/service/     business logic, orchestrates ethereum + cache
internal/ethereum/    go-ethereum client wrapper
internal/cache/       Redis client wrapper
internal/config/      Viper-based configuration loader
internal/logger/      Zap logger constructor
internal/models/      DTOs / response envelope
internal/utils/       address validation, wei<->ether conversion, etc.
tests/                integration-style tests
docs/                 generated Swagger/OpenAPI spec
```

## Configuration

All configuration is environment-driven (see `.env.example` for the full
list). The only required variable is `ETH_RPC_URL`.

## Roadmap

- [x] Milestone 1 — skeleton, config, logging, `/health`
- [ ] Milestone 2 — Ethereum client wrapper + `/blocks/latest`, `/blocks/:number`
- [ ] Milestone 3 — Redis cache layer
- [ ] Milestone 4 — concurrent multi-block fetch, `/blocks/latest/:count`
- [ ] Milestone 5 — transactions, wallet balance/nonce
- [ ] Milestone 6 — gas price, contract code detection
- [ ] Milestone 7 — ERC-20 `balanceOf` via ABI
- [ ] Milestone 8 — event logs
- [ ] Milestone 9 — middleware: logging, recovery, CORS, rate limiting
- [ ] Milestone 10 — Swagger/OpenAPI generation
- [ ] Milestone 11 — unit tests
- [ ] Milestone 12 — Docker, docker-compose, final README
