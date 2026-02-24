package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Embed ABI and Bytecode (generate with solc --abi MusicRoyalty.sol; solc --bin MusicRoyalty.sol)
const musicRoyaltyABI = `[{"inputs":[{"internalType":"address","name":"_artist","type":"address"},{"internalType":"address","name":"_label","type":"address"}],"stateMutability":"nonpayable","type":"constructor"},...]` // Full ABI here
const musicRoyaltyBin = `0x6080604052...`                                                                                                                                                                                 // Full bytecode here

type MusicRoyalty struct { /* Generated fields from abigen */
}

func main() {
	client, err := ethclient.Dial("http://localhost:8545") // Local node
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	privateKey, err := crypto.HexToECDSA("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80") // Anvil default
	if err != nil {
		log.Fatal(err)
	}
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("invalid key")
	}
	fromAddress := crypto.PubkeyToAddress(*publicKeyECDSA)

	nonce, err := client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		log.Fatal(err)
	}
	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	// Deploy contract
	artist := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266") // Anvil account 1
	label := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")  // Anvil account 2
	auth, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
	if err != nil {
		log.Fatal(err)
	}
	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0)

	address, tx, instance, err := bind.DeployMusicRoyalty(auth, client, artist, label)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Contract deployed at %s (tx: %s)\n", address.Hex(), tx.Hash().Hex())

	// Simulate stream payment
	payment := big.NewInt(1e18)                                                                                   // 1 ETH
	txPayment, err := instance.Transfer(binder.TransactOpts{From: fromAddress, Value: payment}, common.Address{}) // Or use receive via sendTransaction
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Payment sent: %s\n", txPayment.Hash().Hex())

	// Read artist share
	artistShare, err := instance.ArtistShare(nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Artist share: %d%%\n", artistShare)
}
