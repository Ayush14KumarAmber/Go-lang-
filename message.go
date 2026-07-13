package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hyperledger/fabric-contract-api-go/contractapi"
)

type SmartContract struct {
	contractapi.Contract
}

type MessageInfo struct {
	Sender    string `json:"sender"`
	Message   string `json:"message"`
	Amount    int64  `json:"amount"`
	Timestamp int64  `json:"timestamp"`
}

// SendMessage stores a message with payment information
func (s *SmartContract) SendMessage(
	ctx contractapi.TransactionContextInterface,
	message string,
	amount int64,
	timestamp int64,
) error {

	if message == "" {
		return fmt.Errorf("message cannot be empty")
	}

	if amount <= 0 {
		return fmt.Errorf("amount must be greater than zero")
	}

	clientID, err := ctx.GetClientIdentity().GetID()
	if err != nil {
		return err
	}

	txID := ctx.GetStub().GetTxID()

	msg := MessageInfo{
		Sender:    clientID,
		Message:   message,
		Amount:    amount,
		Timestamp: timestamp,
	}

	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return ctx.GetStub().PutState(txID, msgJSON)
}

// GetAllMessages returns every stored message
func (s *SmartContract) GetAllMessages(ctx contractapi.TransactionContextInterface) ([]*MessageInfo, error) {

	resultsIterator, err := ctx.GetStub().GetStateByRange("", "")
	if err != nil {
		return nil, err
	}
	defer resultsIterator.Close()

	var messages []*MessageInfo

	for resultsIterator.HasNext() {
		queryResult, err := resultsIterator.Next()
		if err != nil {
			return nil, err
		}

		var msg MessageInfo
		err = json.Unmarshal(queryResult.Value, &msg)
		if err != nil {
			return nil, err
		}

		messages = append(messages, &msg)
	}

	return messages, nil
}

// GetMessageCount returns total number of messages
func (s *SmartContract) GetMessageCount(ctx contractapi.TransactionContextInterface) (int, error) {

	resultsIterator, err := ctx.GetStub().GetStateByRange("", "")
	if err != nil {
		return 0, err
	}
	defer resultsIterator.Close()

	count := 0

	for resultsIterator.HasNext() {
		_, err := resultsIterator.Next()
		if err != nil {
			return 0, err
		}
		count++
	}

	return count, nil
}

// GetMessageByTxID fetches a specific message
func (s *SmartContract) GetMessageByTxID(ctx contractapi.TransactionContextInterface, txID string) (*MessageInfo, error) {

	data, err := ctx.GetStub().GetState(txID)
	if err != nil {
		return nil, err
	}

	if data == nil {
		return nil, fmt.Errorf("message not found")
	}

	var msg MessageInfo
	err = json.Unmarshal(data, &msg)
	if err != nil {
		return nil, err
	}

	return &msg, nil
}

// DeleteMessage removes a message
func (s *SmartContract) DeleteMessage(ctx contractapi.TransactionContextInterface, txID string) error {
	return ctx.GetStub().DelState(txID)
}

func (s *SmartContract) InitLedger(ctx contractapi.TransactionContextInterface) error {
	return ctx.GetStub().PutState("messageCounter", []byte(strconv.Itoa(0)))
}

func main() {
	chaincode, err := contractapi.NewChaincode(new(SmartContract))
	if err != nil {
		panic(err.Error())
	}

	if err := chaincode.Start(); err != nil {
		panic(err.Error())
	}
}
