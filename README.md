# Instructions
A simple repo to filter all BNB auctions and submit a empty bid to the auction. The bid will 100% fail since bidAmount is too low. The repo is just used to verify if the bid is received by atlas or not. 

## 1. export env vars
```
export PRIVATE_KEY=0xprivatekey
export SOLVER_CONTRACT=0xsolver_ address
```
## 2. run the script
```
go run .
```
## 3. query the bid
```
go run . query auctionId userOpHash
```
e.g.
```
go run . query 4ec698e3-da74-4e4e-829e-2de5190e6832 0xc00cc9ca24b548d2e96acc95d0488ee6deea779c5eb730d31d5e94bc8916764d
```
## 4 See result
There should be something you can see in the terminal, means the gateway receive the bid.
```
2026/03/06 13:13:58 Querying https://solver-query-api-fra.fastlane-labs.xyz/ ...
2026/03/06 13:13:59 Status: 200 OK
2026/03/06 13:13:59 Response:
{
  "jsonrpc": "2.0",
  "result": {
    "auctionId": "bd5534b6-a273-47a0-bd39-46a713aeb655",
    "userOperationHash": "0x4c8843bec577bdeb5167171481bb9181f818c002018fe5af244dbf1ace2fb631",
    "solverOperationFrom": "0x6302fb2BA585f5B444A20c02aFa838bD91b81e1E",
    "result": "solver operation validation failedsolver operation bid amount is too low",
    "signature": ""
  },
  "id": "query-1"
}
```