package main

import (
    "context"
    "fmt"
    "log"
    "math/big"
    "net/http"
    "os"

    "github.com/ethereum/go-ethereum/accounts/abi/bind"
    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/ethclient"
    "github.com/gin-gonic/gin"
    "voting-backend/contracts" // auto-generated from abigen
)

func main() {
    // Connect to node (Ganache/Infura)
    client, err := ethclient.Dial("HTTP://127.0.0.1:7545") // or Infura URL
    if err != nil {
        log.Fatal(err)
    }

    contractAddress := common.HexToAddress("0xYourDeployedContractAddress")
    instance, err := contracts.NewVoting(contractAddress, client)
    if err != nil {
        log.Fatal(err)
    }

    router := gin.Default()

    // GET: Fetch votes for a candidate
    router.GET("/votes/:id", func(c *gin.Context) {
        id := c.Param("id")
        index := new(big.Int)
        index.SetString(id, 10)
        votes, err := instance.GetVotes(&bind.CallOpts{}, index)
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusOK, gin.H{"candidate_id": id, "votes": votes})
    })

    // POST: Cast a vote
    router.POST("/vote/:id", func(c *gin.Context) {
        privateKey := os.Getenv("PRIVATE_KEY") // load from .env
        auth, _ := bind.NewTransactorWithChainID(nil, privateKey, big.NewInt(1337)) // Example ChainID

        id := c.Param("id")
        index := new(big.Int)
        index.SetString(id, 10)

        tx, err := instance.Vote(auth, index)
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusOK, gin.H{"tx_hash": tx.Hash().Hex()})
    })

    router.Run(":8080")
}
