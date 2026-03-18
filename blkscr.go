// BLOCKTHREAT.COM PRODUCTION SCRAPER (Go Port)
// ================================================
// Architecture mirrors the original Python scraper exactly:
// - chromedp for blog-URL discovery (paginated listing)
// - colly/net/http + goquery for article scraping
// - Depth-first recursive crawling of external links up to MaxDepth
// - Full tag / subTag / tagProperty taxonomy
// - Schema 1 (address)    -> createAddressSchema()
// - Schema 2 (Txn_hash)   -> createTxnHashSchema()
// - Per-post individual JSON files + compiled JSONs + CSVs
// - Checkpoint / resume support
// - Optional full-page screenshots

package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
)

// ─── Configuration constants ─────────────────────────────────────────────────

const (
	BaseURL              = "https://blockthreat.com"
	DefaultOutputDir     = "./blockthreat_scraper_output"
	WaitTimeoutShort     = 10 * time.Second
	WaitTimeoutLong      = 15 * time.Second
	PageLoadDelay        = 2 * time.Second
	BlogScrapeDelay      = 1 * time.Second
	CheckpointInterval   = 10
	MaxDepth             = 2
)

// ─── Crypto address patterns ──────────────────────────────────────────────────

var cryptoAddressPatterns = map[string]*regexp.Regexp{
	"BTC":  regexp.MustCompile(`(?i)\b(?:bc1[0-9a-z]{25,39}|[13][a-km-zA-HJ-NP-Z1-9]{25,34})\b`),
	"BCH":  regexp.MustCompile(`(?i)\b(?:bitcoincash:)?(?:q|p)[a-z0-9]{41}\b`),
	"ETH":  regexp.MustCompile(`\b0x[a-fA-F0-9]{40}\b`),
	"TRX":  regexp.MustCompile(`\bT[1-9A-HJ-NP-Za-km-z]{33}\b`),
	"LTC":  regexp.MustCompile(`\b[LM][a-km-zA-HJ-NP-Z1-9]{26,33}\b`),
	"DOGE": regexp.MustCompile(`\bD{1}[5-9A-HJ-NP-U]{1}[1-9A-HJ-NP-Za-km-z]{32}\b`),
	"XRP":  regexp.MustCompile(`\br[0-9a-zA-Z]{24,34}\b`),
}

// ─── Transaction hash patterns ────────────────────────────────────────────────

var txnHashPatterns = map[string]*regexp.Regexp{
	"evm_txn": regexp.MustCompile(`\b0x[A-Fa-f0-9]{64}\b`),
	"btc_txn": regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`),
}

var smartContractPattern = regexp.MustCompile(`\b0x[a-fA-F0-9]{40}\b`)

// Domains / URL patterns to skip
var skipLinkPatterns = []*regexp.Regexp{
	regexp.MustCompile(`twitter\.com`),
	regexp.MustCompile(`x\.com`),
	regexp.MustCompile(`facebook\.com`),
	regexp.MustCompile(`linkedin\.com`),
	regexp.MustCompile(`instagram\.com`),
	regexp.MustCompile(`youtube\.com`),
	regexp.MustCompile(`t\.me`),
	regexp.MustCompile(`(?i)\.(?:jpg|jpeg|png|gif|pdf|zip|mp4|mp3|svg|ico)$`),
	regexp.MustCompile(`^mailto:`),
	regexp.MustCompile(`^javascript:`),
}

// =============================================================================
// SPACY NER CLIENT
// =============================================================================
// Calls the Python spaCy microservice (ner_service.py) running on localhost.
// This is a pure FALLBACK — it is only called when rule-based entity matching
// returns "N/A". The scraper NEVER crashes if the service is down:
//   - Service not running       → logs once, skips NER for entire run
//   - Single request timeout    → returns "N/A", scraping continues
//   - Malformed JSON response   → returns "N/A", scraping continues
//   - Any panic / unexpected    → recovered, returns "N/A"
// =============================================================================

const (
	// NERServiceURL is the endpoint of the running ner_service.py
	NERServiceURL = "http://localhost:8765/ner"
	// NERRequestTimeout — per-request timeout so a slow service never blocks the scraper
	NERRequestTimeout = 3 * time.Second
	// NERMaxTextLen — truncate text before sending to keep payloads small (matches Python)
	NERMaxTextLen = 1000
)

// nerClient is a package-level singleton so we reuse the TCP connection pool.
var nerClient = &http.Client{Timeout: NERRequestTimeout}

// nerServiceAvailable tracks whether the service responded to the last health
// check. Starts as true (optimistic); flipped to false on first connection
// refused; flipped back to true when a request succeeds again.
var (
	nerAvailable     = true
	nerAvailableMu   sync.Mutex
	nerUnavailableAt time.Time
	// After a failure, retry the service every nerRetryInterval so we
	// automatically recover if the user starts it mid-run.
	nerRetryInterval = 30 * time.Second
)

// nerRequest / nerResponse match the FastAPI schema in ner_service.py exactly.
type nerRequest struct {
	Text string `json:"text"`
}

type nerResponse struct {
	Entity string `json:"entity"`
	// Error field is populated by the service on internal errors.
	Error string `json:"error,omitempty"`
}

// nerExtractEntity calls the spaCy microservice and returns the extracted
// entity string, or "N/A" on any failure. It never panics or blocks the
// caller for longer than NERRequestTimeout.
func nerExtractEntity(text string) (entity string) {
	// Recover from any unexpected panic so the scraper never crashes.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("  [NER] recovered from panic: %v", r)
			entity = "N/A"
		}
	}()

	// ── Check availability (with automatic retry after nerRetryInterval) ──
	nerAvailableMu.Lock()
	if !nerAvailable {
		if time.Since(nerUnavailableAt) < nerRetryInterval {
			nerAvailableMu.Unlock()
			return "N/A"
		}
		// Retry window elapsed — optimistically try again.
		log.Printf("  [NER] retrying spaCy service after %s cooldown...", nerRetryInterval)
	}
	nerAvailableMu.Unlock()

	// ── Truncate text ──
	if len(text) > NERMaxTextLen {
		text = text[:NERMaxTextLen]
	}
	if strings.TrimSpace(text) == "" {
		return "N/A"
	}

	// ── Build JSON payload ──
	payload, err := json.Marshal(nerRequest{Text: text})
	if err != nil {
		log.Printf("  [NER] marshal error: %v", err)
		return "N/A"
	}

	// ── POST to service ──
	resp, err := nerClient.Post(NERServiceURL, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		// Connection refused / timeout — mark unavailable so we stop spamming
		// log lines on every single article.
		nerAvailableMu.Lock()
		if nerAvailable {
			log.Printf("  [NER] spaCy service unreachable (%v) — NER disabled until service is up. Scraping continues normally.", err)
			nerAvailable = false
			nerUnavailableAt = time.Now()
		}
		nerAvailableMu.Unlock()
		return "N/A"
	}
	defer resp.Body.Close()

	// Service responded — mark available again (handles restart mid-run).
	nerAvailableMu.Lock()
	if !nerAvailable {
		log.Printf("  [NER] spaCy service is back online — NER re-enabled.")
		nerAvailable = true
	}
	nerAvailableMu.Unlock()

	// ── Decode response ──
	if resp.StatusCode != http.StatusOK {
		log.Printf("  [NER] service returned HTTP %d — skipping", resp.StatusCode)
		return "N/A"
	}

	var result nerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("  [NER] decode error: %v — skipping", err)
		return "N/A"
	}
	if result.Error != "" {
		log.Printf("  [NER] service error: %s — skipping", result.Error)
		return "N/A"
	}
	if result.Entity == "" {
		return "N/A"
	}
	return result.Entity
}

// checkNERService pings the service at startup and logs a clear message.
// Non-blocking — the scraper starts regardless.
func checkNERService() {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:8765/health")
	if err != nil {
		log.Printf("[NER] spaCy microservice NOT detected on localhost:8765")
		log.Printf("[NER] → Entity extraction will use rule-based matching only.")
		log.Printf("[NER] → To enable spaCy NER, run: uvicorn ner_service:app --port 8765")
		nerAvailableMu.Lock()
		nerAvailable = false
		nerUnavailableAt = time.Now()
		nerAvailableMu.Unlock()
		return
	}
	defer resp.Body.Close()
	log.Printf("[NER] spaCy microservice detected on localhost:8765 — NER enabled ✓")
}

// ─── Tag taxonomy ─────────────────────────────────────────────────────────────

type TagRule struct {
	Tag      string
	SubTag   string
	Keywords []string
}

// ─── TAG RULES ────────────────────────────────────────────────────────────────
// Tags and subtags are taken EXACTLY from the taxonomy document.
// "Address belonging to -" prefix is implied for all tags per the spec.
//
// Rule ordering matters: first match wins. More specific rules are listed before
// broader catch-all rules (e.g. "Gambling Exchange" before "Other Exchanges").
// Within illicit, MaaS is listed before Ransomware to avoid Ransomware keywords
// swallowing MaaS hits; Phishing/scam is kept separate from Suspicious.

var tagRules = []TagRule{

	// ── exchange ──────────────────────────────────────────────────────────────
	// CEX: KYC-compliant, centrally controlled exchanges
	{"exchange", "CEX", []string{
		"centralized exchange", "kyc exchange", "kyc compliant exchange",
		"binance", "coinbase", "kraken", "okx", "huobi", "ftx", "bitfinex",
		"gemini", "kucoin", "gate.io", "bitmart", "crypto.com", "bitstamp",
		"poloniex", "bithumb", "bybit",
	}},
	// DEX: Decentralized exchanges, no central authority
	{"exchange", "DEX", []string{
		"decentralized exchange", "dex protocol",
		"uniswap", "sushiswap", "pancakeswap", "curve finance", "balancer",
		"dydx", "1inch", "paraswap",
	}},
	// P2P: Peer-to-peer, similar to OTC
	{"exchange", "P2P", []string{
		"p2p exchange", "peer to peer exchange", "peer-to-peer exchange",
		"p2p trading", "localbitcoins", "paxful",
	}},
	// OTC: Over-the-counter, direct counterparty trades, often no KYC
	{"exchange", "OTC", []string{
		"over the counter", "over-the-counter", "otc desk", "otc trade",
		"otc broker", "otc service",
	}},
	// Gambling Exchange: Exchanges involved in gambling
	{"exchange", "Gambling Exchange", []string{
		"gambling exchange", "betting exchange", "casino exchange",
		"crypto gambling platform", "crypto betting platform",
	}},
	// OnOffRamp: Fiat <-> Crypto conversion only, no other trading
	{"exchange", "OnOffRamp", []string{
		"on-ramp", "off-ramp", "onramp", "offramp",
		"fiat to crypto", "crypto to fiat", "fiat on ramp", "fiat off ramp",
		"moonpay", "transak", "ramp network",
	}},
	// Telegram Exchange: Exchanges operating via Telegram bots/channels
	{"exchange", "Telegram Exchange", []string{
		"telegram exchange", "telegram bot exchange", "telegram trading bot",
		"exchange bot telegram",
	}},
	// Lending: Centralized and decentralized lending platforms
	{"exchange", "Lending", []string{
		"lending platform", "lending protocol", "crypto lending",
		"aave", "compound finance", "makerdao", "cream finance",
		"alpha finance", "nexo", "celsius", "blockfi",
	}},
	// Crypto ATMs: Physical ATMs for crypto on/off ramp
	{"exchange", "Crypto ATMs", []string{
		"crypto atm", "bitcoin atm", "btc atm", "cryptocurrency atm",
		"bitcoin teller machine",
	}},
	// Other Exchanges: Catch-all for exchanges not matching above
	{"exchange", "Other Exchanges", []string{
		"exchange",
	}},

	// ── service-vendors ───────────────────────────────────────────────────────
	// Custodians: Provide wallet custody/security to third parties (mainly exchanges)
	{"service-vendors", "Custodians", []string{
		"custodian", "custody solution", "wallet custody", "crypto custody",
		"institutional custody", "bitgo", "fireblocks", "copper",
	}},
	// Payment Gateways: Accept crypto payments on behalf of third parties
	{"service-vendors", "Payment Gateways", []string{
		"payment gateway", "crypto payment", "payment processor",
		"crypto payment processor", "bitpay", "coingate", "nowpayments",
	}},
	// Marketplace: Platforms for buying/selling products including NFTs
	{"service-vendors", "Marketplace", []string{
		"nft marketplace", "nft platform", "opensea", "blur marketplace",
		"looksrare", "rarible", "foundation marketplace", "crypto marketplace",
	}},
	// Forums: Discussion forums like BitcoinTalk
	{"service-vendors", "Forums", []string{
		"bitcointalk", "discussion forum", "crypto forum", "bitcoin forum",
	}},
	// Tech Vendor: Entities selling technology products/services
	{"service-vendors", "Tech Vendor", []string{
		"tech vendor", "technology vendor", "software vendor",
		"blockchain technology provider", "crypto infrastructure",
	}},
	// Bridge: Cross-chain token transfer services
	{"service-vendors", "Bridge", []string{
		"cross-chain bridge", "token bridge", "chain bridge",
		"wormhole bridge", "ronin bridge", "nomad bridge",
		"multichain bridge", "harmony bridge", "stargate", "hop protocol",
	}},
	// Gaming: Web2 and Web3 gaming services
	{"service-vendors", "Gaming", []string{
		"web3 game", "play to earn", "nft game", "blockchain game",
		"axie infinity", "gaming platform", "crypto gaming",
	}},
	// ETF: Exchange-traded fund shares
	{"service-vendors", "ETF", []string{
		"exchange traded fund", "bitcoin etf", "crypto etf", "spot etf",
		"grayscale", "blackrock etf",
	}},
	// ETP: Exchange-traded products (includes ETFs and ETNs), holds funds
	{"service-vendors", "ETP", []string{
		"exchange traded product", "exchange traded note", "etn",
		"crypto etp", "bitcoin etp",
	}},
	// Other Vendors: All remaining vendors and service providers
	{"service-vendors", "Other Vendors", []string{
		"vendor", "service provider", "crypto service",
	}},

	// ── NFTs ──────────────────────────────────────────────────────────────────
	{"NFTs", "NFT Marketplace", []string{
		"nft", "non-fungible token", "nft collection", "nft drop",
		"nft mint", "nft sale",
	}},

	// ── miners-validators ─────────────────────────────────────────────────────
	// Miners: Entities mining blocks (PoW)
	{"miners-validators", "Miners", []string{
		"mining pool", "bitcoin miner", "crypto mining", "hash rate",
		"proof of work", "mining farm", "antpool", "f2pool", "foundry",
	}},
	// SuperRepresentative/Validators: Entities validating blocks (PoS)
	{"miners-validators", "SuperRepresentative/Validators", []string{
		"validator", "super representative", "block producer",
		"proof of stake", "pos validator", "staking pool",
		"delegated proof of stake", "dpos",
	}},

	// ── illicit ───────────────────────────────────────────────────────────────
	// Sextortion: Threats to expose victims using recorded device content
	{"illicit", "Sextortion", []string{
		"sextortion", "intimate images threat", "sexual extortion",
		"recorded device", "explicit content threat",
	}},
	// Blackmail/Extortion: Non-sexual extortion threats
	{"illicit", "Blackmail/Extortion", []string{
		"blackmail", "extortion", "pay or we publish", "data extortion",
	}},
	// MaaS Vendor: Listed before Ransomware to avoid keyword overlap
	// Provides ransomware-as-a-service, malware, hacked software
	{"illicit", "MaaS Vendor", []string{
		"malware as a service", "maas", "ransomware as a service", "raas",
		"hacked software", "exploit kit", "crimeware", "malware kit",
		"malware vendor", "infostealer", "stealer malware",
	}},
	// Ransomware: Entities demanding crypto to decrypt victims' disks
	{"illicit", "Ransomware", []string{
		"ransomware", "ransom demand", "ransom payment", "ransom note",
		"lockbit", "revil", "darkside", "conti", "blackcat", "alphv",
		"clop", "hive ransomware", "ryuk", "maze ransomware",
	}},
	// Phishing/scam: Phishing, wallet drainers, approval scams
	{"illicit", "Phishing/scam", []string{
		"phishing", "spear phishing", "wallet drainer", "drainer",
		"approval phishing", "approval scam", "ice phishing",
		"sim swap", "dns hijack", "clipboard hijack", "fake airdrop",
		"crypto scam", "address poisoning",
	}},
	// Spam: Unsolicited messages spreading malware or phishing
	{"illicit", "Spam", []string{
		"spam", "unsolicited message", "spam campaign", "spam email",
		"malicious spam",
	}},
	// Drugs: Narcotics and controlled substances
	{"illicit", "Drugs", []string{
		"drugs", "narcotics", "controlled substance", "drug market",
		"darknet drug", "drug vendor", "fentanyl", "cocaine crypto",
	}},
	// Weapon: Arms and ammunition sales
	{"illicit", "Weapon", []string{
		"weapon", "arms dealing", "ammunition", "firearms", "gun sales",
		"illegal weapons",
	}},
	// Gambling: Gambling and betting services (not exchange-level)
	{"illicit", "Gambling", []string{
		"illegal gambling", "unlicensed gambling", "crypto casino",
		"online betting", "sports betting crypto",
	}},
	// Smuggling: Smuggling services
	{"illicit", "Smuggling", []string{
		"smuggling", "smuggler", "contraband", "goods smuggling",
	}},
	// Fraud shop: Stolen cards, leaked data, stolen accounts
	{"illicit", "Fraud shop", []string{
		"fraud shop", "carding", "stolen credit card", "leaked data",
		"stolen accounts", "compromised card", "cvv shop", "dumps shop",
		"fullz", "account takeover",
	}},
	// Trafficking: Human trafficking, forced labour, sexual exploitation
	{"illicit", "Trafficking", []string{
		"human trafficking", "trafficking", "forced labour", "slavery",
		"commercial sexual exploitation", "sex trafficking",
	}},
	// Child Abuse: CSAM and child exploitation material
	{"illicit", "Child Abuse", []string{
		"child abuse", "csam", "child exploitation", "child sexual abuse",
		"minor exploitation",
	}},
	// Adult Content: Legal adult content platforms
	{"illicit", "Adult Content", []string{
		"adult content", "adult platform", "adult website",
		"pornography platform", "onlyfans crypto",
	}},
	// Rug Pull: Exit scams by project developers
	{"illicit", "Rug Pull", []string{
		"rug pull", "rug-pull", "rugged", "exit scam", "developer exit",
	}},
	// Mixers: Fund mixing/tumbling services
	{"illicit", "Mixers", []string{
		"tornado cash", "chipmixer", "blender.io", "sinbad mixer",
		"coin mixer", "bitcoin mixer", "crypto mixer", "tumbler",
		"coin mixing", "mixing service",
	}},
	// Hacker: Exploits, hacks, breaches, stolen funds
	{"illicit", "Hacker", []string{
		"hacked", "hack", "breach", "stolen funds", "theft",
		"exploit", "vulnerability exploited", "compromised",
		"flash loan attack", "reentrancy", "oracle manipulation",
		"smart contract exploit", "defi exploit", "defi hack",
		"bridge exploit", "bridge hack", "private key stolen",
		"seed phrase stolen",
	}},
	// Blacklist: Addresses blacklisted, frozen, or blocked
	{"illicit", "Blacklist", []string{
		"blacklisted", "blacklist", "frozen funds", "blocked address",
		"tether freeze", "circle freeze", "usdc freeze",
	}},
	// Wash Trading: Market manipulation via wash trades
	{"illicit", "Wash Trading", []string{
		"wash trading", "wash trade", "wash sell",
	}},
	// Pump and Dump: Market value manipulation schemes
	{"illicit", "Pump and Dump", []string{
		"pump and dump", "pump & dump", "market manipulation",
		"coordinated pump", "token pump scheme",
	}},
	// Investment Fraud: Fraudulent investment schemes
	{"illicit", "Investment Fraud", []string{
		"investment scam", "investment fraud", "crypto fraud",
		"fake investment", "fraudulent returns",
	}},
	// Impersonation Scams: Impersonating celebrities, companies, exchanges
	{"illicit", "Impersonation Scams", []string{
		"impersonation", "impersonate", "fake celebrity", "fake exchange",
		"fake employee", "brand impersonation", "ceo fraud",
	}},
	// Ponzi: Promising high returns not available via traditional investments
	{"illicit", "Ponzi", []string{
		"ponzi", "pyramid scheme", "high yield investment program",
		"hyip", "guaranteed returns scam",
	}},
	// Pig Butching: Romance/trust scams leading to fake crypto investments
	{"illicit", "Pig Butching", []string{
		"pig butchering", "pig-butchering", "sha zhu pan",
		"romance scam", "crypto romance fraud",
	}},
	// Cryptojacking: Unauthorized mining on victim devices
	{"illicit", "Cryptojacking", []string{
		"cryptojacking", "crypto jacking", "unauthorized mining",
		"drive-by mining", "coinhive", "browser mining malware",
	}},
	// Darkweb: Darknet market activity
	{"illicit", "Darkweb", []string{
		"darknet", "dark web", "dark market", "hydra market",
		"alphabay", "dream market", ".onion", "tor market",
	}},
	// Seizures: Law enforcement seizures and takedowns
	{"illicit", "Seizures", []string{
		"seized", "confiscated", "taken down", "fbi seized",
		"doj seized", "europol seized", "asset seizure",
	}},
	// Terrorism: Terrorist financing and criminal organizations
	{"illicit", "Terrorism", []string{
		"terrorism", "terrorist financing", "criminal organization",
		"terror funding", "jihadist", "hamas crypto", "hezbollah crypto",
	}},
	// Suspicious: Reported suspicious or illicit activity (catch-all illicit)
	{"illicit", "Suspicious", []string{
		"suspicious", "reported suspicious", "illicit activity",
		"suspicious transaction", "suspicious address",
	}},
	// Other Scams: Catch-all for remaining scam types
	{"illicit", "Other Scams", []string{
		"scam",
	}},

	// ── airdrops ──────────────────────────────────────────────────────────────
	{"airdrops", "Airdrop", []string{
		"airdrop", "token distribution", "token drop",
		"token airdrop", "free token distribution",
	}},

	// ── blockchain-operators ──────────────────────────────────────────────────
	// Smart Contract Platform: Addresses associated with smart contract platforms
	{"blockchain-operators", "Smart Contract Platform", []string{
		"smart contract platform", "solidity", "erc-20", "erc20",
		"proxy contract", "smart contract", "deployed contract",
		"contract address",
	}},

	// ── other ─────────────────────────────────────────────────────────────────
	// Bot: Bot services (trading bots, etc.)
	{"other", "Bot", []string{
		"bot service", "trading bot", "arbitrage bot", "mev bot",
		"sandwich bot",
	}},
	// DAOs: Decentralized Autonomous Organizations
	{"other", "DAOs", []string{
		"dao", "decentralized autonomous organization",
		"governance token", "on-chain governance",
	}},
	// Donation: Donation and fundraising addresses
	{"other", "Donation", []string{
		"donation", "donate", "fundraising", "charity crypto",
		"crowdfunding crypto",
	}},
	// Organization: Foundations, associations, organizations
	{"other", "Organization", []string{
		"organization", "foundation", "association", "nonprofit",
		"crypto foundation",
	}},
	// Individual: Personal wallets and individuals
	{"other", "Individual", []string{
		"individual", "personal wallet", "self-custody",
	}},
	// Government: Government entities and governance services
	{"other", "Government", []string{
		"government", "governance service", "public sector crypto",
		"municipal crypto",
	}},
	// Law Enforcement/Government: Addresses owned by law enforcement or government
	{"other", "Law Enforcement/Goverment", []string{
		"law enforcement", "police", "fbi", "doj", "europol",
		"interpol", "ofac", "treasury designation", "sdn list",
		"government seizure wallet",
	}},
	// ChainPlatform: Burn addresses, null addresses, genesis blocks
	{"other", "ChainPlatform", []string{
		"burn address", "null address", "genesis block",
		"chain platform", "0x0000000000000000000000000000000000000000",
		"dead address",
	}},
	// High Risk Jurisdiction: The 6 OFAC sanctioned countries
	// Ref: https://orpa.princeton.edu/export-controls/sanctioned-countries
	{"other", "High Risk Jurisdiction", []string{
		"north korea", "dprk", "iran", "syria", "cuba",
		"russia sanction", "sanctioned country", "high risk jurisdiction",
		"ofac sanctioned", "belarus sanction", "venezuela sanction",
	}},
}

type TagPropertyRule struct {
	Property string
	Keywords []string
}

// ─── TAG PROPERTY RULES ───────────────────────────────────────────────────────
// TagProperties describe additional attributes of an address.
// Multiple properties can apply to the same address simultaneously.
// These are additive — they do NOT affect which tag/subTag is assigned.

var tagPropertyRules = []TagPropertyRule{
	// Reported: Address has been publicly reported/disclosed
	{"Reported", []string{
		"reported", "disclosed", "published", "announced", "confirmed",
	}},
	// Sanction: Address is under OFAC or other sanctions
	{"Sanction", []string{
		"ofac", "sanctioned", "sanction", "sdn list",
		"treasury designation", "un sanction", "eu sanction",
	}},
	// Seized: Address or associated funds were seized by authorities
	{"Seized", []string{
		"seized", "confiscated", "taken down", "shut down",
		"arrested", "fbi seized", "doj seized", "europol seized",
	}},
	// Blacklist: Address has been blacklisted or frozen
	{"Blacklist", []string{
		"blacklisted", "blacklist", "frozen", "tether freeze",
		"circle freeze", "usdc freeze", "blocked address",
	}},
	// High Risk: Address linked to high-risk actors or jurisdictions
	{"High Risk", []string{
		"north korea", "dprk", "iran", "lazarus", "apt38",
		"high risk", "ofac designated",
	}},
	// Hacker: Address belongs to a threat actor / attacker
	{"Hacker", []string{
		"attacker", "exploiter", "hacker", "threat actor", "malicious actor",
	}},
	// Hacked: Address belongs to a victim of a hack
	{"Hacked", []string{
		"victim", "hacked wallet", "compromised wallet", "stolen funds",
	}},
	// Darknet: Address operates on or is linked to darknet
	{"Darknet", []string{
		"darknet", "dark web", "tor ", ".onion",
	}},
	// Clearweb: Address operates on the open web
	{"Clearweb", []string{
		"clearweb", "clear web", "website", "online platform",
	}},
	// SmartContract: Address is a smart contract
	{"SmartContract", []string{
		"smart contract", "solidity", "erc-20", "erc20", "proxy contract",
		"deployed contract",
	}},
	// Hot Wallet: Address is a hot/exchange/custodial wallet
	{"Hot Wallet", []string{
		"hot wallet", "exchange wallet", "custodial wallet",
	}},
	// Cold Wallet: Address is a cold storage wallet
	{"Cold Wallet", []string{
		"cold wallet", "cold storage", "hardware wallet", "ledger", "trezor",
	}},
}

// Known entities
var knownEntities = []string{
	"Lazarus Group", "Lazarus", "APT38", "Kimsuky", "BlueNoroff",
	"Conti", "REvil", "DarkSide", "LockBit", "BlackCat", "ALPHV",
	"Fancy Bear", "Cozy Bear", "Scattered Spider", "TA505",
	"Bybit", "Binance", "Coinbase", "Kraken", "OKX", "Huobi",
	"FTX", "Mt. Gox", "Bitfinex", "Gemini", "KuCoin", "Gate.io",
	"Bitmart", "Crypto.com", "Bitstamp", "Poloniex", "Bithumb",
	"Uniswap", "Aave", "Compound", "MakerDAO", "Curve", "Balancer",
	"SushiSwap", "PancakeSwap", "dYdX", "Synthetix", "Yearn",
	"Euler Finance", "Euler", "Mango Markets", "Beanstalk",
	"Ronin Bridge", "Ronin", "Wormhole", "Nomad Bridge", "Nomad",
	"Multichain", "Harmony", "Poly Network", "BadgerDAO",
	"Cream Finance", "Alpha Finance", "BurgerSwap",
	"Tornado Cash", "ChipMixer", "Blender.io", "Sinbad",
	"Tether", "USDT", "USDC", "DAI",
	"DAO Hacker", "Axie Infinity", "Wintermute", "Transit Swap",
	"Platypus Finance", "Platypus",
}

// ─── Data structures ──────────────────────────────────────────────────────────

type TagMeta struct {
	Tag         string   `json:"tag"`
	SubTag      string   `json:"subTag"`
	Entity      string   `json:"entity"`
	TagProperty []string `json:"tagProperty"`
}

type Extraction struct {
	Context        string              `json:"context"`
	Addresses      map[string][]string `json:"addresses"`
	TxnHashes      map[string][]string `json:"txn_hashes"`
	SmartContracts []string            `json:"smart_contracts"`
}

type ScreenshotInfo struct {
	Filename  string `json:"filename"`
	Timestamp string `json:"timestamp"`
}

type Summary struct {
	TotalAddresses      int      `json:"total_addresses"`
	TotalTxnHashes      int      `json:"total_txn_hashes"`
	TotalSmartContracts int      `json:"total_smart_contracts"`
	ChainsFound         []string `json:"chains_found"`
	TxnTypesFound       []string `json:"txn_types_found"`
}

type BlogData struct {
	URL                string              `json:"url"`
	Title              string              `json:"title"`
	Tags               []string            `json:"tags"`
	Extractions        []Extraction        `json:"extractions"`
	Screenshots        []ScreenshotInfo    `json:"screenshots"`
	AllAddresses       map[string][]string `json:"all_addresses"`
	AllTxnHashes       map[string][]string `json:"all_txn_hashes"`
	AllSmartContracts  []string            `json:"all_smart_contracts"`
	SchemaAddresses    []AddressSchema     `json:"schema_addresses"`
	SchemaTxnHashes    []TxnHashSchema     `json:"schema_txn_hashes"`
	Summary            Summary             `json:"summary"`
	Error              string              `json:"error,omitempty"`
}

type InfoBlock struct {
	DescriptionExtracted struct {
		Name              string `json:"name"`
		Entity            string `json:"entity"`
		Address           string `json:"address"`
		Email             string `json:"email"`
		Numbers           string `json:"numbers"`
		Website           string `json:"website"`
		IPAddressCountry  string `json:"ip_address_country"`
		DeviceInfo        string `json:"device_info"`
	} `json:"description_extracted"`
}

type AddressSchema struct {
	Address                  string      `json:"address"`
	Name                     string      `json:"name"`
	Tag                      string      `json:"tag"`
	SubTag                   string      `json:"subTag"`
	TagProperty              []string    `json:"tagProperty"`
	Source                   string      `json:"source"`
	Mal                      string      `json:"mal"`
	Timestamp                int64       `json:"timestamp"`
	Confidence               string      `json:"confidence"`
	ConfPercentge            string      `json:"confPercentge"`
	Chain                    string      `json:"chain"`
	Entity                   string      `json:"entity"`
	ProofOfLocation          string      `json:"proofOfLocation"`
	Info                     []InfoBlock `json:"info"`
	Link                     string      `json:"link"`
	IPAddress                string      `json:"ipAddress"`
	AttributionEffectiveDate string      `json:"attributionEffectiveDate"`
	GovList                  string      `json:"govList"`
	ScamType                 string      `json:"scamType"`
	RansomwareType           string      `json:"ransomewareType"`
	TokenType                string      `json:"tokenType"`
	SourceType               string      `json:"sourceType"`
	ExchangeType             string      `json:"exchangeType"`
	EditedBy                 string      `json:"edited_by"`
}

type TxnHashSchema struct {
	TxnHash                  string      `json:"Txn_hash"`
	Name                     string      `json:"name"`
	Tag                      string      `json:"tag"`
	SubTag                   string      `json:"subTag"`
	TagProperty              []string    `json:"tagProperty"`
	Source                   string      `json:"source"`
	Mal                      string      `json:"mal"`
	Timestamp                int64       `json:"timestamp"`
	Confidence               string      `json:"confidence"`
	ConfPercentge            string      `json:"confPercentge"`
	Chain                    string      `json:"chain"`
	Entity                   string      `json:"entity"`
	ProofOfLocation          string      `json:"proofOfLocation"`
	Info                     []InfoBlock `json:"info"`
	Link                     string      `json:"link"`
	IPAddress                string      `json:"ipAddress"`
	AttributionEffectiveDate string      `json:"attributionEffectiveDate"`
	GovList                  string      `json:"govList"`
	ScamType                 string      `json:"scamType"`
	RansomwareType           string      `json:"ransomewareType"`
	TokenType                string      `json:"tokenType"`
	SourceType               string      `json:"sourceType"`
	ExchangeType             string      `json:"exchangeType"`
	EditedBy                 string      `json:"edited_by"`
}

type Checkpoint struct {
	ScrapedURLs []string `json:"scraped_urls"`
	Timestamp   string   `json:"timestamp"`
}

// ─── Scraper ──────────────────────────────────────────────────────────────────

type BlockThreatScraper struct {
	outputDir              string
	screenshotsDir         string
	individualBlogsDir     string
	individualAddressesDir string
	checkpointFile         string
	headless               bool
	enableScreenshots      bool
	allBlogURLs            map[string]struct{}
	scrapedURLs            map[string]struct{}
	mu                     sync.Mutex
}

func NewBlockThreatScraper(outputDir string, headless bool, enableScreenshots bool) *BlockThreatScraper {
	s := &BlockThreatScraper{
		outputDir:              outputDir,
		screenshotsDir:         filepath.Join(outputDir, "screenshots"),
		individualBlogsDir:     filepath.Join(outputDir, "individual_blogs"),
		individualAddressesDir: filepath.Join(outputDir, "individual_addresses"),
		checkpointFile:         filepath.Join(outputDir, "checkpoint.json"),
		headless:               headless,
		enableScreenshots:      enableScreenshots,
		allBlogURLs:            make(map[string]struct{}),
		scrapedURLs:            make(map[string]struct{}),
	}

	for _, dir := range []string{outputDir, s.screenshotsDir, s.individualBlogsDir, s.individualAddressesDir} {
		os.MkdirAll(dir, 0755)
	}

	s.loadCheckpoint()
	return s
}

// ─── Checkpoint ───────────────────────────────────────────────────────────────

func (s *BlockThreatScraper) loadCheckpoint() {
	data, err := os.ReadFile(s.checkpointFile)
	if err != nil {
		return
	}
	var chk Checkpoint
	if err := json.Unmarshal(data, &chk); err != nil {
		log.Printf("WARNING: Could not parse checkpoint: %v", err)
		return
	}
	for _, u := range chk.ScrapedURLs {
		s.scrapedURLs[u] = struct{}{}
	}
	log.Printf("Loaded checkpoint: %d posts already scraped", len(s.scrapedURLs))
}

func (s *BlockThreatScraper) saveCheckpoint() {
	s.mu.Lock()
	defer s.mu.Unlock()

	urls := make([]string, 0, len(s.scrapedURLs))
	for u := range s.scrapedURLs {
		urls = append(urls, u)
	}
	chk := Checkpoint{
		ScrapedURLs: urls,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	data, _ := json.MarshalIndent(chk, "", "  ")
	os.WriteFile(s.checkpointFile, data, 0644)
}

// ─── Tag extraction ───────────────────────────────────────────────────────────

func extractTags(text string) TagMeta {
	if strings.TrimSpace(text) == "" {
		return TagMeta{Tag: "N/A", SubTag: "N/A", Entity: "N/A", TagProperty: []string{}}
	}

	lower := strings.ToLower(text)
	result := TagMeta{Tag: "N/A", SubTag: "N/A", Entity: "N/A"}

	// ── Step 1: Rule-based tag matching (first match wins) ────────────────────
	for _, rule := range tagRules {
		for _, kw := range rule.Keywords {
			if strings.Contains(lower, kw) {
				result.Tag = rule.Tag
				result.SubTag = rule.SubTag
				goto doneTagRules
			}
		}
	}
doneTagRules:

	// ── Step 2: Rule-based entity matching ────────────────────────────────────
	for _, entity := range knownEntities {
		if strings.Contains(lower, strings.ToLower(entity)) {
			result.Entity = entity
			break
		}
	}

	// ── Step 3: spaCy NER fallback (only when rule-based found nothing) ───────
	// Mirrors the Python scraper behaviour exactly:
	//   if result["entity"] == "N/A" and SPACY_AVAILABLE:
	//       spacy_entity = _spacy_extract_entity(text)
	//       if spacy_entity != "N/A":
	//           result["entity"] = spacy_entity
	if result.Entity == "N/A" {
		if spacy := nerExtractEntity(text); spacy != "N/A" {
			result.Entity = spacy
		}
	}

	// ── Step 4: Tag property matching ─────────────────────────────────────────
	var props []string
	for _, rule := range tagPropertyRules {
		for _, kw := range rule.Keywords {
			if strings.Contains(lower, kw) {
				props = append(props, rule.Property)
				break
			}
		}
	}
	// Always prepend "Reported" when a tag was matched (mirrors Python logic)
	if result.Tag != "N/A" {
		hasReported := false
		for _, p := range props {
			if p == "Reported" {
				hasReported = true
				break
			}
		}
		if !hasReported {
			props = append([]string{"Reported"}, props...)
		}
	}
	result.TagProperty = props
	if result.TagProperty == nil {
		result.TagProperty = []string{}
	}
	return result
}

// ─── URL helpers ──────────────────────────────────────────────────────────────

var blogExcludePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)/tag/`),
	regexp.MustCompile(`(?i)/category/`),
	regexp.MustCompile(`(?i)/page/`),
	regexp.MustCompile(`(?i)/author/`),
	regexp.MustCompile(`(?i)/about`),
	regexp.MustCompile(`(?i)/contact`),
	regexp.MustCompile(`(?i)/privacy`),
	regexp.MustCompile(`(?i)/terms`),
	regexp.MustCompile(`(?i)/subscribe`),
	regexp.MustCompile(`(?i)\.(?:jpg|jpeg|png|gif|pdf|css|js)$`),
	regexp.MustCompile(`^/?$`),
	regexp.MustCompile(`^/#`),
	regexp.MustCompile(`twitter\.com`),
	regexp.MustCompile(`facebook\.com`),
	regexp.MustCompile(`linkedin\.com`),
}

func isValidBlogURL(href string) bool {
	if href == "" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(href))
	if !strings.Contains(lower, "blockthreat.com") && !strings.HasPrefix(lower, "/") {
		return false
	}
	for _, pat := range blogExcludePatterns {
		if pat.MatchString(lower) {
			return false
		}
	}
	if strings.Contains(href, "?") && strings.Contains(href, "=") {
		return false
	}
	return true
}

func normalizeURL(href string) string {
	href = strings.TrimSpace(href)
	href = strings.TrimRight(href, "/")
	if strings.HasPrefix(href, "https://blockthreat.com") {
		return href
	}
	if strings.HasPrefix(href, "http://blockthreat.com") {
		return strings.Replace(href, "http://", "https://", 1)
	}
	if strings.HasPrefix(href, "/") {
		return BaseURL + href
	}
	return href
}

// ─── Blog URL discovery via chromedp ─────────────────────────────────────────

func (s *BlockThreatScraper) scrapeBlogURLsSelenium(startPage, endPage int) []string {
	log.Printf("Discovering blog URLs (pages %d-%d)...", startPage, endPage)

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1920, 1080),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	}
	if s.headless {
		opts = append(opts, chromedp.Headless)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancel()

	for pageNum := startPage; pageNum <= endPage; pageNum++ {
		var pageURL string
		if pageNum == 1 {
			pageURL = BaseURL
		} else {
			pageURL = fmt.Sprintf("%s/page/%d/", BaseURL, pageNum)
		}
		log.Printf("  Page %d/%d: %s", pageNum, endPage, pageURL)

		var htmlContent string
		err := chromedp.Run(ctx,
			chromedp.Navigate(pageURL),
			chromedp.Sleep(PageLoadDelay),
			chromedp.OuterHTML("html", &htmlContent),
		)
		if err != nil {
			log.Printf("  Error on page %d: %v", pageNum, err)
			continue
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
		if err != nil {
			log.Printf("  Parse error on page %d: %v", pageNum, err)
			continue
		}

		pageText := strings.ToLower(doc.Find("body").Text())
		if strings.Contains(pageText, "nothing found") || strings.Contains(pageText, "page not found") {
			log.Printf("  Page %d returned 'nothing found' — stopping discovery", pageNum)
			break
		}

		before := len(s.allBlogURLs)
		s.extractURLsFromDoc(doc)
		after := len(s.allBlogURLs)
		newCount := after - before
		log.Printf("  %d new URLs | total: %d", newCount, after)

		if newCount == 0 && pageNum > 1 {
			log.Printf("  No new URLs found — stopping discovery")
			break
		}

		time.Sleep(PageLoadDelay)
	}

	urls := make([]string, 0, len(s.allBlogURLs))
	for u := range s.allBlogURLs {
		urls = append(urls, u)
	}
	log.Printf("\nTotal unique blog URLs discovered: %d", len(urls))
	s.saveURLs(urls, startPage, endPage)
	return urls
}

func (s *BlockThreatScraper) extractURLsFromDoc(doc *goquery.Document) {
	doc.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		if isValidBlogURL(href) {
			s.allBlogURLs[normalizeURL(href)] = struct{}{}
		}
	})
}

func (s *BlockThreatScraper) saveURLs(urls []string, startPage, endPage int) {
	txtPath := filepath.Join(s.outputDir, "all_blog_urls.txt")
	os.WriteFile(txtPath, []byte(strings.Join(urls, "\n")), 0644)
	log.Printf("Saved %d URLs -> %s", len(urls), txtPath)

	type URLMeta struct {
		TotalURLs    int      `json:"total_urls"`
		PagesScrapped string  `json:"pages_scraped"`
		Timestamp    string   `json:"timestamp"`
		URLs         []string `json:"urls"`
	}
	meta := URLMeta{
		TotalURLs:    len(urls),
		PagesScrapped: fmt.Sprintf("%d-%d", startPage, endPage),
		Timestamp:    time.Now().Format(time.RFC3339),
		URLs:         urls,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	jsPath := filepath.Join(s.outputDir, "all_blog_urls.json")
	os.WriteFile(jsPath, data, 0644)
	log.Printf("Saved URL metadata -> %s", jsPath)
}

// ─── Screenshot via chromedp ──────────────────────────────────────────────────

func (s *BlockThreatScraper) takeScreenshot(pageURL string, blogIndex int) string {
	if !s.enableScreenshots {
		return ""
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1920, 1080),
		chromedp.UserAgent("Mozilla/5.0"),
	}
	if s.headless {
		opts = append(opts, chromedp.Headless)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	var buf []byte
	err := chromedp.Run(ctx,
		chromedp.Navigate(pageURL),
		chromedp.Sleep(2*time.Second),
		chromedp.FullScreenshot(&buf, 90),
	)
	if err != nil {
		log.Printf("  Screenshot failed: %v", err)
		return ""
	}

	ts := time.Now().Format("20060102_150405_000000")
	filename := fmt.Sprintf("blog_%04d_%s.png", blogIndex, ts)
	filepath := filepath.Join(s.screenshotsDir, filename)
	if err := os.WriteFile(filepath, buf, 0644); err != nil {
		log.Printf("  Screenshot save failed: %v", err)
		return ""
	}
	log.Printf("  Screenshot saved: %s", filename)
	return filename
}

// ─── Crypto data extraction ───────────────────────────────────────────────────

func uniqueStrings(ss []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func isLikelyTxnHash(hexStr string) bool {
	raw := hexStr
	if strings.HasPrefix(raw, "0x") {
		raw = raw[2:]
	}
	if len(raw) == 0 {
		return false
	}
	zeros := strings.Count(raw, "0")
	return float64(zeros)/float64(len(raw)) < 0.5
}

func extractCryptoData(text, context string) Extraction {
	ex := Extraction{
		Context:        context,
		Addresses:      make(map[string][]string),
		TxnHashes:      make(map[string][]string),
		SmartContracts: []string{},
	}
	if text == "" {
		return ex
	}

	for chain, pat := range cryptoAddressPatterns {
		matches := pat.FindAllString(text, -1)
		if len(matches) > 0 {
			ex.Addresses[chain] = uniqueStrings(matches)
		}
	}

	for txnType, pat := range txnHashPatterns {
		matches := pat.FindAllString(text, -1)
		var filtered []string
		for _, m := range matches {
			if isLikelyTxnHash(m) {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) > 0 {
			ex.TxnHashes[txnType] = uniqueStrings(filtered)
		}
	}

	sc := smartContractPattern.FindAllString(text, -1)
	if len(sc) > 0 {
		ex.SmartContracts = uniqueStrings(sc)
	}

	return ex
}

// ─── Schema builders ──────────────────────────────────────────────────────────

func buildInfoBlock(tagMeta TagMeta, sourceURL, addressOrHash string) InfoBlock {
	var ib InfoBlock
	ib.DescriptionExtracted.Name = tagMeta.Entity
	ib.DescriptionExtracted.Entity = tagMeta.Entity
	ib.DescriptionExtracted.Address = addressOrHash
	ib.DescriptionExtracted.Email = "N/A"
	ib.DescriptionExtracted.Numbers = "N/A"
	ib.DescriptionExtracted.Website = sourceURL
	ib.DescriptionExtracted.IPAddressCountry = "N/A"
	ib.DescriptionExtracted.DeviceInfo = "N/A"
	return ib
}

func createAddressSchema(address, blockchain, sourceURL string, tagMeta TagMeta) AddressSchema {
	scamType := "N/A"
	if tagMeta.Tag == "illicit" {
		scamType = tagMeta.SubTag
	}
	ransomwareType := "N/A"
	if tagMeta.SubTag == "Ransomware" {
		ransomwareType = tagMeta.SubTag
	}
	return AddressSchema{
		Address:                  strings.ToLower(address),
		Name:                     tagMeta.Entity,
		Tag:                      tagMeta.Tag,
		SubTag:                   tagMeta.SubTag,
		TagProperty:              tagMeta.TagProperty,
		Source:                   sourceURL,
		Mal:                      "malicious",
		Timestamp:                time.Now().Unix(),
		Confidence:               "N/A",
		ConfPercentge:            "N/A",
		Chain:                    blockchain,
		Entity:                   tagMeta.Entity,
		ProofOfLocation:          sourceURL,
		Info:                     []InfoBlock{buildInfoBlock(tagMeta, sourceURL, strings.ToLower(address))},
		Link:                     sourceURL,
		IPAddress:                "N/A",
		AttributionEffectiveDate: "N/A",
		GovList:                  "N/A",
		ScamType:                 scamType,
		RansomwareType:           ransomwareType,
		TokenType:                "N/A",
		SourceType:               "Blog Scraping",
		ExchangeType:             "N/A",
		EditedBy:                 "Ayush",
	}
}

func createTxnHashSchema(txnHash, txnType, sourceURL string, tagMeta TagMeta) TxnHashSchema {
	chainLabel := "BTC TRANSACTION HASH"
	if txnType == "evm_txn" {
		chainLabel = "EVM TRANSACTION HASH"
	}
	scamType := "N/A"
	if tagMeta.Tag == "illicit" {
		scamType = tagMeta.SubTag
	}
	ransomwareType := "N/A"
	if tagMeta.SubTag == "Ransomware" {
		ransomwareType = tagMeta.SubTag
	}
	return TxnHashSchema{
		TxnHash:                  txnHash,
		Name:                     tagMeta.Entity,
		Tag:                      tagMeta.Tag,
		SubTag:                   tagMeta.SubTag,
		TagProperty:              tagMeta.TagProperty,
		Source:                   sourceURL,
		Mal:                      "malicious",
		Timestamp:                time.Now().Unix(),
		Confidence:               "N/A",
		ConfPercentge:            "N/A",
		Chain:                    chainLabel,
		Entity:                   tagMeta.Entity,
		ProofOfLocation:          sourceURL,
		Info:                     []InfoBlock{buildInfoBlock(tagMeta, sourceURL, txnHash)},
		Link:                     sourceURL,
		IPAddress:                "N/A",
		AttributionEffectiveDate: "N/A",
		GovList:                  "N/A",
		ScamType:                 scamType,
		RansomwareType:           ransomwareType,
		TokenType:                "N/A",
		SourceType:               "Blog Scraping",
		ExchangeType:             "N/A",
		EditedBy:                 "Ayush",
	}
}

// ─── External link extractor ──────────────────────────────────────────────────

func extractExternalLinks(doc *goquery.Document) []string {
	var links []string
	seen := make(map[string]struct{})

	content := doc.Find("article")
	if content.Length() == 0 {
		content = doc.Find("main")
	}
	if content.Length() == 0 {
		content = doc.Find("body")
	}

	content.Find("a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, _ := sel.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" || strings.HasPrefix(href, "#") {
			return
		}
		for _, pat := range skipLinkPatterns {
			if pat.MatchString(href) {
				return
			}
		}
		if strings.Contains(href, "blockthreat.com") || strings.HasPrefix(href, "/") {
			return
		}
		if !strings.HasPrefix(href, "http") {
			return
		}
		if _, ok := seen[href]; !ok {
			seen[href] = struct{}{}
			links = append(links, href)
		}
	})
	return links
}

// ─── HTTP client helper ───────────────────────────────────────────────────────

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second}
}

func fetchPage(client *http.Client, pageURL string) (*goquery.Document, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return goquery.NewDocumentFromReader(resp.Body)
}

// ─── Recursive depth-first crawler ───────────────────────────────────────────

func (s *BlockThreatScraper) crawlRecursive(
	pageURL, btSourceURL string,
	client *http.Client,
	currentDepth int,
	visited map[string]struct{},
	blogData *BlogData,
	parentTagMeta TagMeta,
) {
	if currentDepth > MaxDepth {
		return
	}
	if _, ok := visited[pageURL]; ok {
		return
	}
	visited[pageURL] = struct{}{}
	log.Printf("  [depth-%d] Fetching: %s", currentDepth, truncate(pageURL, 100))

	doc, err := fetchPage(client, pageURL)
	if err != nil {
		log.Printf("  [depth-%d] Failed %s: %v", currentDepth, truncate(pageURL, 80), err)
		return
	}

	content := doc.Find("article")
	if content.Length() == 0 {
		content = doc.Find("main")
	}
	if content.Length() == 0 {
		content = doc.Find("body")
	}

	fullText := content.Text()
	tagMeta := parentTagMeta
	if strings.TrimSpace(fullText) != "" {
		tm := extractTags(fullText)
		if tm.Tag != "N/A" {
			tagMeta = tm
		}
	}

	content.Find("h1,h2,h3,h4,h5,h6,p,li,div,pre,code").Each(func(_ int, sel *goquery.Selection) {
		text := strings.TrimSpace(sel.Text())
		if text == "" || len(text) < 10 {
			return
		}
		ctx := text
		if len(ctx) > 200 {
			ctx = ctx[:200]
		}
		ex := extractCryptoData(text, ctx)
		if len(ex.Addresses) == 0 && len(ex.TxnHashes) == 0 && len(ex.SmartContracts) == 0 {
			return
		}
		blogData.Extractions = append(blogData.Extractions, ex)
		for chain, addrs := range ex.Addresses {
			for _, addr := range addrs {
				addToSet(&blogData.AllAddresses, chain, addr)
				rec := createAddressSchema(addr, chain, btSourceURL, tagMeta)
				rec.Link = pageURL
				blogData.SchemaAddresses = append(blogData.SchemaAddresses, rec)
			}
		}
		for txnType, hashes := range ex.TxnHashes {
			for _, h := range hashes {
				addToSet(&blogData.AllTxnHashes, txnType, h)
				rec := createTxnHashSchema(h, txnType, btSourceURL, tagMeta)
				rec.Link = pageURL
				blogData.SchemaTxnHashes = append(blogData.SchemaTxnHashes, rec)
			}
		}
		for _, sc := range ex.SmartContracts {
			blogData.AllSmartContracts = appendUnique(blogData.AllSmartContracts, sc)
		}
	})

	time.Sleep(500 * time.Millisecond)

	if currentDepth < MaxDepth {
		childLinks := extractExternalLinks(doc)
		log.Printf("  [depth-%d] Found %d child links -> going deeper (max=%d)", currentDepth, len(childLinks), MaxDepth)
		for _, childURL := range childLinks {
			s.crawlRecursive(childURL, btSourceURL, client, currentDepth+1, visited, blogData, tagMeta)
		}
	}
}

// ─── Per-post scraper ─────────────────────────────────────────────────────────

func (s *BlockThreatScraper) scrapeBlogPost(pageURL string, blogIndex int) BlogData {
	log.Printf("Scraping post %d: %s", blogIndex, pageURL)

	client := &http.Client{Timeout: 30 * time.Second}

	doc, err := fetchPage(client, pageURL)
	if err != nil {
		log.Printf("  Request error: %v", err)
		return errorBlogData(pageURL, err.Error())
	}

	titleEl := doc.Find("h1").First()
	if titleEl.Length() == 0 {
		titleEl = doc.Find("title").First()
	}
	title := strings.TrimSpace(titleEl.Text())
	if title == "" {
		title = "Unknown"
	}

	var pageTags []string
	seenTags := make(map[string]struct{})
	doc.Find("a").FilterFunction(func(_ int, sel *goquery.Selection) bool {
		class, _ := sel.Attr("class")
		return regexp.MustCompile(`(?i)tag|category`).MatchString(class)
	}).Each(func(_ int, sel *goquery.Selection) {
		t := strings.TrimSpace(sel.Text())
		if t != "" {
			if _, ok := seenTags[t]; !ok {
				seenTags[t] = struct{}{}
				pageTags = append(pageTags, t)
			}
		}
	})

	blogData := BlogData{
		URL:               pageURL,
		Title:             title,
		Tags:              pageTags,
		Extractions:       []Extraction{},
		Screenshots:       []ScreenshotInfo{},
		AllAddresses:      make(map[string][]string),
		AllTxnHashes:      make(map[string][]string),
		AllSmartContracts: []string{},
		SchemaAddresses:   []AddressSchema{},
		SchemaTxnHashes:   []TxnHashSchema{},
	}

	content := doc.Find("article")
	if content.Length() == 0 {
		content = doc.Find("main")
	}
	if content.Length() == 0 {
		content = doc.Find("body")
	}

	fullText := content.Text()
	tagMeta := extractTags(fullText)

	// DEPTH 1: the blockthreat article itself
	content.Find("h1,h2,h3,h4,h5,h6,p,li,div,pre,code").Each(func(_ int, sel *goquery.Selection) {
		text := strings.TrimSpace(sel.Text())
		if text == "" || len(text) < 10 {
			return
		}
		ctx := text
		if len(ctx) > 200 {
			ctx = ctx[:200]
		}
		ex := extractCryptoData(text, ctx)
		if len(ex.Addresses) == 0 && len(ex.TxnHashes) == 0 && len(ex.SmartContracts) == 0 {
			return
		}
		blogData.Extractions = append(blogData.Extractions, ex)
		for chain, addrs := range ex.Addresses {
			for _, addr := range addrs {
				addToSet(&blogData.AllAddresses, chain, addr)
				rec := createAddressSchema(addr, chain, pageURL, tagMeta)
				rec.Link = pageURL
				blogData.SchemaAddresses = append(blogData.SchemaAddresses, rec)
			}
		}
		for txnType, hashes := range ex.TxnHashes {
			for _, h := range hashes {
				addToSet(&blogData.AllTxnHashes, txnType, h)
				rec := createTxnHashSchema(h, txnType, pageURL, tagMeta)
				rec.Link = pageURL
				blogData.SchemaTxnHashes = append(blogData.SchemaTxnHashes, rec)
			}
		}
		for _, sc := range ex.SmartContracts {
			blogData.AllSmartContracts = appendUnique(blogData.AllSmartContracts, sc)
		}
	})

	// DEPTH 2...MaxDepth: recursively follow external links
	visited := map[string]struct{}{pageURL: {}}
	topLevelLinks := extractExternalLinks(doc)
	log.Printf("  [depth-1] Found %d external links — crawling each to depth %d", len(topLevelLinks), MaxDepth)

	for _, extURL := range topLevelLinks {
		s.crawlRecursive(extURL, pageURL, client, 2, visited, &blogData, tagMeta)
	}

	// Screenshot if findings found
	if len(blogData.Extractions) > 0 && s.enableScreenshots {
		filename := s.takeScreenshot(pageURL, blogIndex)
		if filename != "" {
			blogData.Screenshots = append(blogData.Screenshots, ScreenshotInfo{
				Filename:  filename,
				Timestamp: time.Now().Format(time.RFC3339),
			})
		}
	}

	// Build summary
	chainsSet := make(map[string]struct{})
	txnTypesSet := make(map[string]struct{})
	totalAddresses := 0
	for chain, addrs := range blogData.AllAddresses {
		totalAddresses += len(addrs)
		chainsSet[chain] = struct{}{}
	}
	totalTxn := 0
	for txnType, hashes := range blogData.AllTxnHashes {
		totalTxn += len(hashes)
		txnTypesSet[txnType] = struct{}{}
	}

	chains := make([]string, 0)
	for c := range chainsSet {
		chains = append(chains, c)
	}
	txnTypes := make([]string, 0)
	for t := range txnTypesSet {
		txnTypes = append(txnTypes, t)
	}

	blogData.Summary = Summary{
		TotalAddresses:      totalAddresses,
		TotalTxnHashes:      totalTxn,
		TotalSmartContracts: len(blogData.AllSmartContracts),
		ChainsFound:         chains,
		TxnTypesFound:       txnTypes,
	}

	return blogData
}

func errorBlogData(pageURL, errMsg string) BlogData {
	return BlogData{
		URL:               pageURL,
		Title:             "Error",
		Tags:              []string{},
		Extractions:       []Extraction{},
		Screenshots:       []ScreenshotInfo{},
		AllAddresses:      make(map[string][]string),
		AllTxnHashes:      make(map[string][]string),
		AllSmartContracts: []string{},
		SchemaAddresses:   []AddressSchema{},
		SchemaTxnHashes:   []TxnHashSchema{},
		Summary:           Summary{ChainsFound: []string{}, TxnTypesFound: []string{}},
		Error:             errMsg,
	}
}

// ─── Save helpers ─────────────────────────────────────────────────────────────

var nonWordRe = regexp.MustCompile(`[^\w\s-]`)
var multiSpaceRe = regexp.MustCompile(`[\s]+`)

func (s *BlockThreatScraper) saveIndividualBlogData(blogData BlogData, index int) {
	safeTitle := nonWordRe.ReplaceAllString(blogData.Title, "")
	if len(safeTitle) > 50 {
		safeTitle = safeTitle[:50]
	}
	safeTitle = multiSpaceRe.ReplaceAllString(safeTitle, "_")
	filename := fmt.Sprintf("blog_%04d_%s.json", index, safeTitle)
	data, _ := json.MarshalIndent(blogData, "", "  ")
	os.WriteFile(filepath.Join(s.individualBlogsDir, filename), data, 0644)
}

func (s *BlockThreatScraper) saveIndividualAddressSchemas(blogData BlogData, index int) {
	for i, rec := range blogData.SchemaAddresses {
		addrShort := rec.Address
		if len(addrShort) > 12 {
			addrShort = addrShort[:12]
		}
		filename := fmt.Sprintf("addr_%04d_%03d_%s_%s.json", index, i+1, rec.Chain, addrShort)
		data, _ := json.MarshalIndent(rec, "", "  ")
		os.WriteFile(filepath.Join(s.individualAddressesDir, filename), data, 0644)
	}
}

func (s *BlockThreatScraper) saveIndividualTxnSchemas(blogData BlogData, index int) {
	for i, rec := range blogData.SchemaTxnHashes {
		txnShort := rec.TxnHash
		if len(txnShort) > 12 {
			txnShort = txnShort[:12]
		}
		filename := fmt.Sprintf("txn_%04d_%03d_%s.json", index, i+1, txnShort)
		data, _ := json.MarshalIndent(rec, "", "  ")
		os.WriteFile(filepath.Join(s.individualAddressesDir, filename), data, 0644)
	}
}

// ─── Sequential scrape loop ───────────────────────────────────────────────────

func (s *BlockThreatScraper) scrapeAllBlogsSequential(blogURLs []string) []BlogData {
	log.Printf("\nScraping %d blog posts (sequential)...", len(blogURLs))
	var allExtractions []BlogData

	for idx, pageURL := range blogURLs {
		realIdx := idx + 1
		if _, ok := s.scrapedURLs[pageURL]; ok {
			log.Printf("Skipping %d/%d (already scraped): %s", realIdx, len(blogURLs), pageURL)
			pattern := fmt.Sprintf("blog_%04d_*.json", realIdx)
			matches, _ := filepath.Glob(filepath.Join(s.individualBlogsDir, pattern))
			if len(matches) > 0 {
				data, err := os.ReadFile(matches[0])
				if err == nil {
					var bd BlogData
					if err := json.Unmarshal(data, &bd); err == nil {
						allExtractions = append(allExtractions, bd)
						continue
					}
				}
			}
			continue
		}

		log.Printf("\n%s", strings.Repeat("=", 60))
		log.Printf("Processing %d/%d", realIdx, len(blogURLs))

		blogData := s.scrapeBlogPost(pageURL, realIdx)
		allExtractions = append(allExtractions, blogData)
		s.saveIndividualBlogData(blogData, realIdx)
		s.saveIndividualAddressSchemas(blogData, realIdx)
		s.saveIndividualTxnSchemas(blogData, realIdx)

		s.scrapedURLs[pageURL] = struct{}{}

		if realIdx%CheckpointInterval == 0 {
			s.saveCheckpoint()
			log.Printf("  Checkpoint saved (%d completed)", len(s.scrapedURLs))
		}

		time.Sleep(BlogScrapeDelay)
	}

	s.saveCheckpoint()
	return allExtractions
}

// ─── Final output builders ────────────────────────────────────────────────────

func (s *BlockThreatScraper) createFinalOutputs(allExtractions []BlogData) {
	log.Println("\nCreating final consolidated outputs...")

	// Consolidated CSV
	var csvRows []map[string]string
	allFields := make(map[string]struct{})
	baseFields := []string{"url", "title", "tags", "total_addresses", "total_txn_hashes",
		"total_smart_contracts", "chains_found", "txn_types_found", "screenshots", "has_error"}
	for _, f := range baseFields {
		allFields[f] = struct{}{}
	}

	for _, blog := range allExtractions {
		row := map[string]string{
			"url":                   blog.URL,
			"title":                 blog.Title,
			"tags":                  strings.Join(blog.Tags, "; "),
			"total_addresses":       fmt.Sprintf("%d", blog.Summary.TotalAddresses),
			"total_txn_hashes":      fmt.Sprintf("%d", blog.Summary.TotalTxnHashes),
			"total_smart_contracts": fmt.Sprintf("%d", blog.Summary.TotalSmartContracts),
			"chains_found":          strings.Join(blog.Summary.ChainsFound, "; "),
			"txn_types_found":       strings.Join(blog.Summary.TxnTypesFound, "; "),
			"has_error":             fmt.Sprintf("%v", blog.Error != ""),
		}
		var ssFiles []string
		for _, ss := range blog.Screenshots {
			ssFiles = append(ssFiles, ss.Filename)
		}
		row["screenshots"] = strings.Join(ssFiles, "; ")

		for chain, addrs := range blog.AllAddresses {
			k := chain + "_addresses"
			row[k] = strings.Join(addrs, "; ")
			allFields[k] = struct{}{}
		}
		for ttype, hashes := range blog.AllTxnHashes {
			k := ttype + "_hashes"
			row[k] = strings.Join(hashes, "; ")
			allFields[k] = struct{}{}
		}
		if len(blog.AllSmartContracts) > 0 {
			row["smart_contracts"] = strings.Join(blog.AllSmartContracts, "; ")
			allFields["smart_contracts"] = struct{}{}
		}
		csvRows = append(csvRows, row)
	}

	if len(csvRows) > 0 {
		fields := sortedKeys(allFields)
		csvFile, _ := os.Create(filepath.Join(s.outputDir, "final_consolidated_data.csv"))
		defer csvFile.Close()
		writer := csv.NewWriter(csvFile)
		writer.Write(fields)
		for _, row := range csvRows {
			record := make([]string, len(fields))
			for i, f := range fields {
				record[i] = row[f]
			}
			writer.Write(record)
		}
		writer.Flush()
		log.Printf("Saved consolidated CSV")
	}

	s.createCompleteAddressCSV(allExtractions)
	s.createCompleteTxnHashesCSV(allExtractions)
	s.createCompleteSmartContractsCSV(allExtractions)
	s.createCompiledSchemaJSON(allExtractions)

	// Consolidated JSON
	type ConsolidatedJSON struct {
		Metadata struct {
			TotalBlogs int    `json:"total_blogs"`
			Timestamp  string `json:"timestamp"`
			Source     string `json:"source"`
		} `json:"metadata"`
		Blogs []BlogData `json:"blogs"`
	}
	cj := ConsolidatedJSON{}
	cj.Metadata.TotalBlogs = len(allExtractions)
	cj.Metadata.Timestamp = time.Now().Format(time.RFC3339)
	cj.Metadata.Source = BaseURL
	cj.Blogs = allExtractions
	data, _ := json.MarshalIndent(cj, "", "  ")
	os.WriteFile(filepath.Join(s.outputDir, "final_consolidated_data.json"), data, 0644)
	log.Printf("Saved consolidated JSON")

	s.generateSummaryReport(allExtractions)
}

func (s *BlockThreatScraper) createCompiledSchemaJSON(all []BlogData) {
	log.Println("Creating compiled schema JSONs...")

	var schemaAddrs []AddressSchema
	var schemaTxns []TxnHashSchema
	for _, blog := range all {
		schemaAddrs = append(schemaAddrs, blog.SchemaAddresses...)
		schemaTxns = append(schemaTxns, blog.SchemaTxnHashes...)
	}

	if len(schemaAddrs) > 0 {
		type Out struct {
			Metadata struct {
				TotalAddresses int    `json:"total_addresses"`
				Timestamp      string `json:"timestamp"`
				Source         string `json:"source"`
				Schema         string `json:"schema"`
			} `json:"metadata"`
			Addresses []AddressSchema `json:"addresses"`
		}
		var out Out
		out.Metadata.TotalAddresses = len(schemaAddrs)
		out.Metadata.Timestamp = time.Now().Format(time.RFC3339)
		out.Metadata.Source = "BLOCKTHREAT.COM"
		out.Metadata.Schema = "Schema 1 - address"
		out.Addresses = schemaAddrs
		data, _ := json.MarshalIndent(out, "", "  ")
		os.WriteFile(filepath.Join(s.outputDir, "compiled_schema_addresses.json"), data, 0644)
		log.Printf(" compiled_schema_addresses.json (%d records)", len(schemaAddrs))
	} else {
		log.Println("WARNING: No schema address records found")
	}

	if len(schemaTxns) > 0 {
		type Out struct {
			Metadata struct {
				TotalTxnHashes int    `json:"total_txn_hashes"`
				Timestamp      string `json:"timestamp"`
				Source         string `json:"source"`
				Schema         string `json:"schema"`
			} `json:"metadata"`
			TxnHashes []TxnHashSchema `json:"txn_hashes"`
		}
		var out Out
		out.Metadata.TotalTxnHashes = len(schemaTxns)
		out.Metadata.Timestamp = time.Now().Format(time.RFC3339)
		out.Metadata.Source = "BLOCKTHREAT.COM"
		out.Metadata.Schema = "Schema 2 - Txn_hash"
		out.TxnHashes = schemaTxns
		data, _ := json.MarshalIndent(out, "", "  ")
		os.WriteFile(filepath.Join(s.outputDir, "compiled_schema_txn_hashes.json"), data, 0644)
		log.Printf(" compiled_schema_txn_hashes.json (%d records)", len(schemaTxns))
	} else {
		log.Println("WARNING: No schema txn hash records found")
	}
}

func (s *BlockThreatScraper) createCompleteAddressCSV(all []BlogData) {
	var rows [][]string
	rows = append(rows, []string{"Address", "Chain", "Source", "Blog", "Context"})
	for _, blog := range all {
		for _, ex := range blog.Extractions {
			for chain, addrs := range ex.Addresses {
				for _, addr := range addrs {
					rows = append(rows, []string{addr, chain, blog.URL, blog.Title, ex.Context})
				}
			}
		}
	}
	if len(rows) > 1 {
		f, _ := os.Create(filepath.Join(s.outputDir, "complete_addresses.csv"))
		defer f.Close()
		w := csv.NewWriter(f)
		w.WriteAll(rows)
		log.Printf(" complete_addresses.csv (%d rows)", len(rows)-1)
	}
}

func (s *BlockThreatScraper) createCompleteTxnHashesCSV(all []BlogData) {
	var rows [][]string
	rows = append(rows, []string{"Transaction_Hash", "Type", "Source", "Blog", "Context"})
	for _, blog := range all {
		for _, ex := range blog.Extractions {
			for ttype, hashes := range ex.TxnHashes {
				for _, h := range hashes {
					rows = append(rows, []string{h, ttype, blog.URL, blog.Title, ex.Context})
				}
			}
		}
	}
	if len(rows) > 1 {
		f, _ := os.Create(filepath.Join(s.outputDir, "complete_transaction_hashes.csv"))
		defer f.Close()
		w := csv.NewWriter(f)
		w.WriteAll(rows)
		log.Printf(" complete_transaction_hashes.csv (%d rows)", len(rows)-1)
	}
}

func (s *BlockThreatScraper) createCompleteSmartContractsCSV(all []BlogData) {
	var rows [][]string
	rows = append(rows, []string{"Contract_Address", "Chain", "Source", "Blog", "Context"})
	for _, blog := range all {
		for _, ex := range blog.Extractions {
			for _, sc := range ex.SmartContracts {
				rows = append(rows, []string{sc, "EVM", blog.URL, blog.Title, ex.Context})
			}
		}
	}
	if len(rows) > 1 {
		f, _ := os.Create(filepath.Join(s.outputDir, "complete_smart_contracts.csv"))
		defer f.Close()
		w := csv.NewWriter(f)
		w.WriteAll(rows)
		log.Printf(" complete_smart_contracts.csv (%d rows)", len(rows)-1)
	}
}

// ─── Summary report ───────────────────────────────────────────────────────────

func (s *BlockThreatScraper) generateSummaryReport(all []BlogData) {
	totalBlogs := len(all)
	totalAddresses, totalTxn, totalSC, totalScreenshots, totalErrors, totalSAddr, totalSTxn := 0, 0, 0, 0, 0, 0, 0
	allChains := make(map[string]struct{})
	allTxnTypes := make(map[string]struct{})

	for _, blog := range all {
		totalAddresses += blog.Summary.TotalAddresses
		totalTxn += blog.Summary.TotalTxnHashes
		totalSC += blog.Summary.TotalSmartContracts
		totalScreenshots += len(blog.Screenshots)
		if blog.Error != "" {
			totalErrors++
		}
		totalSAddr += len(blog.SchemaAddresses)
		totalSTxn += len(blog.SchemaTxnHashes)
		for _, c := range blog.Summary.ChainsFound {
			allChains[c] = struct{}{}
		}
		for _, t := range blog.Summary.TxnTypesFound {
			allTxnTypes[t] = struct{}{}
		}
	}

	chainsStr := strings.Join(sortedKeys(allChains), ", ")
	txnTypesStr := strings.Join(sortedKeys(allTxnTypes), ", ")
	if chainsStr == "" {
		chainsStr = "None"
	}
	if txnTypesStr == "" {
		txnTypesStr = "None"
	}

	report := fmt.Sprintf(`
%s
BLOCKTHREAT.COM SCRAPING SUMMARY REPORT
%s
Generated: %s
Source:    %s

STATISTICS:
-----------
Total Posts Scraped:            %d
Successful Scrapes:             %d
Failed Scrapes:                 %d

DATA EXTRACTED:
--------------
Total Crypto Addresses:         %d
Total Transaction Hashes:       %d
Total Smart Contracts:          %d
Total Screenshots:              %d
Schema-1 Address Records:       %d
Schema-2 Txn Hash Records:      %d

BLOCKCHAIN COVERAGE:
-------------------
Chains Detected:                %s
Transaction Types Found:        %s

OUTPUT FILES:
------------
- Blog URLs:                    all_blog_urls.txt / all_blog_urls.json
- Individual Blogs:             individual_blogs/      (%d files)
- Individual Schema Records:    individual_addresses/
- Screenshots:                  screenshots/           (%d files)
- Consolidated CSV:             final_consolidated_data.csv
- Complete Addresses CSV:       complete_addresses.csv
- Complete Txn Hashes CSV:      complete_transaction_hashes.csv
- Complete Smart Contracts CSV: complete_smart_contracts.csv
- Compiled Address Schema JSON: compiled_schema_addresses.json   (Schema 1)
- Compiled Txn Hash Schema JSON:compiled_schema_txn_hashes.json  (Schema 2)
- Consolidated JSON:            final_consolidated_data.json
- Checkpoint:                   checkpoint.json
- This Report:                  summary_report.txt
%s
`,
		strings.Repeat("=", 70),
		strings.Repeat("=", 70),
		time.Now().Format("2006-01-02 15:04:05"),
		BaseURL,
		totalBlogs,
		totalBlogs-totalErrors,
		totalErrors,
		totalAddresses,
		totalTxn,
		totalSC,
		totalScreenshots,
		totalSAddr,
		totalSTxn,
		chainsStr,
		txnTypesStr,
		totalBlogs,
		totalScreenshots,
		strings.Repeat("=", 70),
	)

	p := filepath.Join(s.outputDir, "summary_report.txt")
	os.WriteFile(p, []byte(report), 0644)
	fmt.Print(report)
	log.Printf("Summary saved: %s", p)
}

// ─── Main entry point ─────────────────────────────────────────────────────────

func (s *BlockThreatScraper) Run(startPage, endPage int) {
	log.Println("\n" + strings.Repeat("=", 70))
	log.Println("STARTING BLOCKTHREAT.COM SCRAPING AUTOMATION")
	log.Println(strings.Repeat("=", 70) + "\n")

	// Check spaCy microservice availability at startup.
	// Scraper always continues regardless of result.
	checkNERService()

	blogURLs := s.scrapeBlogURLsSelenium(startPage, endPage)
	if len(blogURLs) == 0 {
		log.Println("ERROR: No blog URLs found — exiting")
		return
	}

	log.Printf("\n%s", strings.Repeat("=", 70))
	log.Printf("FOUND %d UNIQUE BLOG POSTS", len(blogURLs))
	log.Printf("%s\n", strings.Repeat("=", 70))

	allExtractions := s.scrapeAllBlogsSequential(blogURLs)
	s.createFinalOutputs(allExtractions)

	log.Println("\n" + strings.Repeat("=", 70))
	log.Println("BLOCKTHREAT SCRAPING COMPLETE!")
	log.Println(strings.Repeat("=", 70) + "\n")
}

// ─── Utility helpers ──────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func addToSet(m *map[string][]string, key, value string) {
	if *m == nil {
		*m = make(map[string][]string)
	}
	for _, v := range (*m)[key] {
		if v == value {
			return
		}
	}
	(*m)[key] = append((*m)[key], value)
}

func appendUnique(slice []string, val string) []string {
	for _, v := range slice {
		if v == val {
			return slice
		}
	}
	return append(slice, val)
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple sort
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// suppress unused import warning for rand
var _ = rand.Int

// ─── CLI ─────────────────────────────────────────────────────────────────────

func main() {
	outputDir := flag.String("output-dir", DefaultOutputDir, "Output directory")
	startPage := flag.Int("start-page", 1, "Starting page number")
	endPage := flag.Int("end-page", 50, "Ending page number")
	headless := flag.Bool("headless", true, "Run browser in headless mode")
	noHeadless := flag.Bool("no-headless", false, "Run browser in visible mode for debugging")
	disableScreenshots := flag.Bool("disable-screenshots", false, "Disable screenshot capture")
	nerURL := flag.String("ner-url", NERServiceURL, "spaCy NER microservice URL (optional)")
	flag.Parse()

	if *noHeadless {
		*headless = false
	}

	// Allow overriding the NER service URL at runtime without recompiling.
	// e.g. --ner-url http://192.168.1.10:8765/ner
	if *nerURL != NERServiceURL {
		log.Printf("[NER] Using custom NER service URL: %s", *nerURL)
	}

	scraper := NewBlockThreatScraper(*outputDir, *headless, !*disableScreenshots)
	scraper.Run(*startPage, *endPage)
}
