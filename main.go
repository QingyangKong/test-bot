package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FastLane-Labs/atlas-sdk-go/config"
	"github.com/FastLane-Labs/atlas-sdk-go/types"
	"github.com/FastLane-Labs/atlas-sdk-go/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

func init() {
	// Register under "1.5" because SDK v0.0.5 only knows versions up to "1.5".
	// The EIP-712 domain still uses the correct "1.6.4" values for signing.
	bnbVersion := config.AtlasV_1_5
	err := config.OverrideChainConfig(bnbChainId, &bnbVersion, &config.ChainConfig{
		Contract: &config.Contract{
			Atlas:             common.HexToAddress("0x21B7d28B882772A1Cfe633Daee6f42ebb95DeC4E"),
			AtlasVerification: common.HexToAddress("0x43b4AAE0F98fC9ebD86A1E9496cDb9D7208EE55B"),
			Sorter:            common.HexToAddress("0x726E9B51FfCC3FD0dA5cf8aB243BE13CEf662582"),
			Simulator:         common.HexToAddress("0x7a28f2c7310454C3440C7b36d59B21aA354b446f"),
			Multicall3:        common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"),
		},
		Eip712Domain: &apitypes.TypedDataDomain{
			Name:              "AtlasVerification",
			Version:           "1.6.4",
			ChainId:           (*math.HexOrDecimal256)(big.NewInt(int64(bnbChainId))),
			VerifyingContract: "0x43b4AAE0F98fC9ebD86A1E9496cDb9D7208EE55B",
		},
	})
	if err != nil {
		log.Fatalf("Failed to override BNB chain config: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const (
	solverNamespace                 = "solver"
	userOperationsSubscriptionTopic = "userOperations"
	submitSolverOperationMethod     = "solver_submitSolverOperation"
	searcherGatewayUrl              = "wss://svr-bid-endpoint.chain.link/ws/solver"
	queryApiUrl                     = "https://solver-query-api-fra.fastlane-labs.xyz/"

	bnbChainId uint64 = 56
)

// BNB Chain DappControl for Chainlink SVR (Venus)
var dappControlAddress = common.HexToAddress("0x7D50b32444609A9B53BcF208c159C8d0d0767835")

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Notification received from the searcher gateway
type UserOperationNotification struct {
	AuctionId            string                           `json:"auction_id"`
	PartialUserOperation *types.UserOperationPartialRaw   `json:"partial_user_operation"`
}

// Solution submitted back to the searcher gateway
type Solution struct {
	AuctionId       string                      `json:"auction_id"`
	AuctionSolution *types.SolverOperationRaw   `json:"auction_solution"`
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	// Load .env (try both go-bot/.env and project root ../.env)
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	pkHex := os.Getenv("PRIVATE_KEY")
	solverContractAddr := os.Getenv("SOLVER_CONTRACT")

	if pkHex == "" {
		log.Fatal("PRIVATE_KEY env var is required")
	}
	if solverContractAddr == "" {
		log.Fatal("SOLVER_CONTRACT env var is required (deployed solver contract address)")
	}

	pkHex = strings.TrimPrefix(pkHex, "0x")
	solverPk, err := crypto.HexToECDSA(pkHex)
	if err != nil {
		log.Fatalf("Invalid PRIVATE_KEY: %v", err)
	}

	solverAccount := crypto.PubkeyToAddress(solverPk.PublicKey)
	solverContract := common.HexToAddress(solverContractAddr)

	log.Println("=== Chainlink SVR Searcher Bot (BNB Chain) ===")
	log.Printf("Solver account  : %s", solverAccount.Hex())
	log.Printf("Solver contract : %s", solverContract.Hex())
	log.Printf("Chain ID        : %d", bnbChainId)
	log.Printf("DappControl     : %s", dappControlAddress.Hex())

	// --------------- Mode selection ---------------
	// Usage:
	//   go run .                                                                          -> subscribe mode
	//   go run . query <auctionId> <userOpHash>                                           -> query (auto-sign)
	//   go run . queryraw <auctionId> <userOpHash> <solverOperationFrom> <signature>      -> query (manual params)
	if len(os.Args) >= 6 && os.Args[1] == "queryraw" {
		queryRaw(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
		return
	}

	if len(os.Args) >= 4 && os.Args[1] == "query" {
		auctionId := os.Args[2]
		userOpHash := os.Args[3]
		queryResult(solverPk, solverAccount, auctionId, userOpHash)
		return
	}

	log.Println("Starting subscribe mode...")
	runSubscriber(solverPk, solverAccount, solverContract)
}

// ---------------------------------------------------------------------------
// Subscribe & Submit
// ---------------------------------------------------------------------------

func runSubscriber(
	solverPk *ecdsa.PrivateKey,
	solverAccount common.Address,
	solverContract common.Address,
) {
	// Set large buffer sizes (auction payloads can exceed default sizes)
	dialerOption := rpc.WithWebsocketDialer(websocket.Dialer{
		ReadBufferSize:  1024 * 1024,
		WriteBufferSize: 1024 * 1024,
	})

	client, err := rpc.DialOptions(context.TODO(), searcherGatewayUrl, dialerOption)
	if err != nil {
		log.Fatalf("Failed to connect to gateway: %v", err)
	}
	log.Println("Connected to SVR Searcher Gateway")

	var (
		uoChan chan *UserOperationNotification
		sub    *rpc.ClientSubscription
	)

	// Subscribe / Resubscribe
	subscribe := func() {
		for {
			uoChan = make(chan *UserOperationNotification, 32)
			sub, err = client.Subscribe(
				context.TODO(),
				solverNamespace,
				uoChan,
				userOperationsSubscriptionTopic,
			)
			if err != nil {
				log.Printf("Subscribe failed: %v, retrying in 1s...", err)
				time.Sleep(1 * time.Second)
				continue
			}
			log.Println("Subscribed to userOperations")
			break
		}
	}

	subscribe()

	log.Println("Waiting for auction notifications...")

	// Main event loop
	for {
		select {
		case subErr := <-sub.Err():
			log.Printf("Subscription error: %v, resubscribing...", subErr)
			subscribe()

		case n := <-uoChan:
			fmt.Printf("\n=== Received Notification ===\n")
			// if you want to print the notification in a pretty format, uncomment the following code
			// data, err := json.MarshalIndent(n, "", "  ")
			// if err != nil {
			// 	fmt.Printf("JSON error: %v\n", err)
			// 	fmt.Printf("%+v\n", n)  // 降级 fallback
			// } else {
			// 	fmt.Println(string(data))
			// }
			
			// Filter: only BNB Chain + Venus DappControl
			if !isOfInterest(n) {
				continue
			}

			log.Println("========================================")
			log.Printf("Received Venus auction on BNB Chain")
			log.Printf("Auction ID : %s", n.AuctionId)
			log.Printf("UserOpHash : %s", n.PartialUserOperation.UserOpHash.Hex())

			// Log feed info from hints
			logHints(n)

			// Build and submit solution
			solution, err := buildSolution(n, solverPk, solverAccount, solverContract)
			if err != nil {
				log.Printf("ERROR building solution: %v", err)
				continue
			}

			// ─────────────── print the solver operation payload ───────────────
			fmt.Println("\n" + strings.Repeat("=", 60))
			fmt.Printf("即将提交的 Solver Operation Payload (Auction: %s)\n", n.AuctionId)
			fmt.Printf("Time: %s\n", time.Now().Format(time.RFC3339Nano))

			payloadBytes, err := json.MarshalIndent(solution, "", "  ")
			if err != nil {
				fmt.Printf("JSON marshal error: %v\n", err)
				fmt.Printf("%+v\n", solution)
			} else {
				fmt.Println(string(payloadBytes))
			}

			// // Test delay: wait before submitting (set to 0 to disable)
			// const submitDelay = 2 * time.Second
			// if submitDelay > 0 {
			// 	log.Printf("Waiting %v before submitting...", submitDelay)
			// 	time.Sleep(submitDelay)
			// }

			var submitResult json.RawMessage
			err = client.CallContext(
				context.TODO(),
				&submitResult,
				submitSolverOperationMethod,
				solution,
			)
			if err != nil {
				log.Printf("ERROR submitting solution: %v", err)
				continue
			}

			log.Printf("Bid submitted! Response: %s", string(submitResult))
			log.Println("--- Save these for querying ---")
			log.Printf("  auctionId          : %s", n.AuctionId)
			log.Printf("  userOperationHash  : %s", n.PartialUserOperation.UserOpHash.Hex())
			log.Printf("  solverOperationFrom: %s", solverAccount.Hex())
			log.Printf("  Query command:")
			log.Printf("    go run . query %s %s", n.AuctionId, n.PartialUserOperation.UserOpHash.Hex())
			log.Println("========================================")
		}
	}
}

// ---------------------------------------------------------------------------
// Filter
// ---------------------------------------------------------------------------

func isOfInterest(n *UserOperationNotification) bool {
	// Check chain ID
	chainId := n.PartialUserOperation.ChainId.ToInt()
	if chainId.Uint64() != bnbChainId {
		return false
	}

	// Check DappControl (Venus on BNB Chain)
	if n.PartialUserOperation.Control != dappControlAddress {
		return false
	}

	return true
}

// ---------------------------------------------------------------------------
// Build Solution
// ---------------------------------------------------------------------------

func buildSolution(
	n *UserOperationNotification,
	solverPk *ecdsa.PrivateKey,
	solverAccount common.Address,
	solverContract common.Address,
) (*Solution, error) {
	// For testing: empty solver data (will cause SolverOpReverted, but proves the flow works)
	// In production, encode your liquidation call here, e.g.:
	//   solverData = abi.encode(LiqBotSolverV2.requestFlashLoan(...))
	solverData := []byte{}

	solverOperation := &types.SolverOperation{
		// The solver account (signer of this operation)
		From: solverAccount,

		// The Atlas address (from the user operation)
		To: n.PartialUserOperation.To,

		// Gas limit for the solver operation
		Gas: big.NewInt(500_000),

		// Must be >= the user operation's maxFeePerGas
		MaxFeePerGas: new(big.Int).Set(n.PartialUserOperation.MaxFeePerGas.ToInt()),

		// Match the user operation's deadline
		Deadline: new(big.Int).Set(n.PartialUserOperation.Deadline.ToInt()),

		// The deployed solver contract address
		Solver: solverContract,

		// The dAppControl address (from user operation)
		Control: n.PartialUserOperation.Control,

		// The user operation hash
		UserOpHash: n.PartialUserOperation.UserOpHash,

		// Bid token: address(0) = native BNB
		BidToken: common.Address{},

		// Bid amount: 0 for testing
		BidAmount: big.NewInt(0),

		// Solver execution data
		Data: solverData,
	}

	// Get chain ID and Atlas version for EIP-712 signing
	chainId := n.PartialUserOperation.ChainId.ToInt().Uint64()

	atlasVersion, err := config.GetVersionFromAtlasAddress(chainId, n.PartialUserOperation.To)
	if err != nil {
		return nil, fmt.Errorf("get atlas version: %w", err)
	}

	// Compute EIP-712 hash of the solver operation
	hash, err := solverOperation.Hash(chainId, &atlasVersion)
	if err != nil {
		return nil, fmt.Errorf("hash solver operation: %w", err)
	}

	// Sign with the solver private key
	signature, err := utils.SignMessage(hash.Bytes(), solverPk)
	if err != nil {
		return nil, fmt.Errorf("sign solver operation: %w", err)
	}
	solverOperation.Signature = signature

	return &Solution{
		AuctionId:       n.AuctionId,
		AuctionSolution: solverOperation.EncodeToRaw(),
	}, nil
}

// ---------------------------------------------------------------------------
// Hints logging
// ---------------------------------------------------------------------------

func logHints(n *UserOperationNotification) {
	hints := n.PartialUserOperation.Hints
	if hints == nil {
		log.Println("Hints: (none)")
		return
	}

	// Extract aggregator (feed) address
	if agg, ok := hints["aggregator"]; ok {
		if aggStr, ok := agg.(string); ok {
			feedAddr := common.HexToAddress(aggStr)
			log.Printf("Feed aggregator: %s", feedAddr.Hex())
		}
	}

	// Extract median price
	if mp, ok := hints["medianPrice"]; ok {
		if mpStr, ok := mp.(string); ok {
			priceHex := strings.TrimPrefix(strings.ToLower(mpStr), "0x")
			priceBytes, err := hex.DecodeString(priceHex)
			if err == nil {
				price := new(big.Int).SetBytes(priceBytes)
				log.Printf("Median price   : %s", price.String())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Query API
// ---------------------------------------------------------------------------

// queryResult queries the Fastlane solver query API for the result of a
// previously submitted solver operation.
//
// Usage: go run . query <auctionId> <userOpHash>
func queryResult(
	solverPk *ecdsa.PrivateKey,
	solverAccount common.Address,
	auctionId string,
	userOpHash string,
) {
	log.Println("=== Query Solver Operation Result ===")
	log.Printf("Auction ID : %s", auctionId)
	log.Printf("UserOpHash : %s", userOpHash)
	log.Printf("Solver     : %s", solverAccount.Hex())

	// Build EIP-191 signature: sign("<auctionId>:<userOpHash>:<solverFrom>")
	// auctionIdNew := "984ce441-c4d7-448d-988e-bcb75877eed1";
	message := fmt.Sprintf("%s:%s:%s", auctionId, userOpHash, solverAccount.Hex())
	signature, err := utils.SignEthereumMessage([]byte(message), solverPk)
	if err != nil {
		log.Fatalf("Failed to sign query message: %v", err)
	}
	sigHex := "0x" + hex.EncodeToString(signature)

	// Build JSON-RPC request
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "query-1",
		"method":  "solver_getSolverOperationResult",
		"params": []interface{}{
			map[string]string{
				"auctionId":           auctionId,
				"userOperationHash":   userOpHash,
				"solverOperationFrom": solverAccount.Hex(),
				"signature":           sigHex,
			},
		},
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		log.Fatalf("Failed to marshal request: %v", err)
	}

	// ─────────────── print the query payload ───────────────
	fmt.Println("\n" + strings.Repeat("=", 70))
	fmt.Printf("[QUERY PAYLOAD PREVIEW] Auction: %s | UserOpHash: %s\n", auctionId, userOpHash)
	fmt.Printf("Time: %s\n", time.Now().Format(time.RFC3339Nano))

	// pretty print the complete JSON payload
	var prettyJSON bytes.Buffer
	err = json.Indent(&prettyJSON, reqBytes, "", "  ")
	if err != nil {
		fmt.Println("Indent failed, showing raw:")
		fmt.Println(string(reqBytes))
	} else {
		fmt.Println("Payload to be sent:")
		fmt.Println(prettyJSON.String())
	}

	fmt.Println(strings.Repeat("=", 70) + "\n")

	log.Printf("Querying %s ...", queryApiUrl)

	resp, err := http.Post(queryApiUrl, "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		log.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response: %v", err)
	}

	log.Printf("Status: %s", resp.Status)

	// Pretty-print the JSON response
	var prettyJSONResponse bytes.Buffer
	if err := json.Indent(&prettyJSONResponse, body, "", "  "); err != nil {
		log.Printf("Response: %s", string(body))
	} else {
		log.Printf("Response:\n%s", prettyJSONResponse.String())
	}
}

// queryRaw queries the Fastlane solver query API with all parameters provided directly.
//
// Usage: go run . queryraw <auctionId> <userOpHash> <solverOperationFrom> <signature>
func queryRaw(auctionId, userOpHash, solverOperationFrom, signature string) {
	log.Println("=== Query Solver Operation Result (raw) ===")
	log.Printf("Auction ID          : %s", auctionId)
	log.Printf("UserOpHash          : %s", userOpHash)
	log.Printf("SolverOperationFrom : %s", solverOperationFrom)
	log.Printf("Signature           : %s", signature)

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "solver_getSolverOperationResult",
		"params": []interface{}{
			map[string]string{
				"auctionId":           auctionId,
				"userOperationHash":   userOpHash,
				"solverOperationFrom": solverOperationFrom,
				"signature":           signature,
			},
		},
	}

	reqBytes, err := json.Marshal(reqBody)
	if err != nil {
		log.Fatalf("Failed to marshal request: %v", err)
	}

	prettyReq, _ := json.MarshalIndent(reqBody, "", "  ")
	fmt.Printf("\nRequest:\n%s\n\n", string(prettyReq))

	log.Printf("Querying %s ...", queryApiUrl)

	resp, err := http.Post(queryApiUrl, "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		log.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response: %v", err)
	}

	log.Printf("Status: %s", resp.Status)

	var prettyResp bytes.Buffer
	if err := json.Indent(&prettyResp, body, "", "  "); err != nil {
		log.Printf("Response: %s", string(body))
	} else {
		log.Printf("Response:\n%s", prettyResp.String())
	}
}
