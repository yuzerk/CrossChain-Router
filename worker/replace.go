package worker

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/anyswap/CrossChain-Router/v3/cmd/utils"
	"github.com/anyswap/CrossChain-Router/v3/common"
	"github.com/anyswap/CrossChain-Router/v3/mongodb"
	"github.com/anyswap/CrossChain-Router/v3/mpc"
	"github.com/anyswap/CrossChain-Router/v3/params"
	"github.com/anyswap/CrossChain-Router/v3/router"
	"github.com/anyswap/CrossChain-Router/v3/tokens"
)

var (
	serverCfg *params.RouterServerConfig

	treatAsNoncePassedInterval = int64(600) // seconds
	defWaitTimeToReplace       = int64(300) // seconds
	defMaxReplaceCount         = 20
)

// StartReplaceJob replace job
func StartReplaceJob() {
	logWorker("replace", "start router swap replace job")
	serverCfg = params.GetRouterServerConfig()
	if serverCfg == nil {
		logWorker("replace", "stop replace swap job as no router server config exist")
		return
	}
	if !serverCfg.EnableReplaceSwap {
		logWorker("replace", "stop replace swap job as disabled")
		return
	}

	allChainIDs := router.AllChainIDs
	mongodb.MgoWaitGroup.Add(len(allChainIDs))
	for _, toChainID := range allChainIDs {
		go doReplaceJob(toChainID.String())
	}
}

func doReplaceJob(toChainID string) {
	defer mongodb.MgoWaitGroup.Done()
	logWorker("replace", "start router swap replace job", "toChainID", toChainID)
	for {
		res, err := findRouterSwapResultToReplace(toChainID)
		if err != nil {
			logWorkerError("replace", "find router swap result error", err, "toChainID", toChainID)
		}
		for _, swap := range res {
			if utils.IsCleanuping() {
				logWorker("replace", "stop router swap replace job", "toChainID", toChainID)
				return
			}
			err = processRouterSwapReplace(swap)
			if err != nil {
				logWorkerError("replace", "process router swap replace error", err, "fromChainID", swap.FromChainID, "toChainID", toChainID, "txid", swap.TxID, "logIndex", swap.LogIndex)
			}
		}
		if utils.IsCleanuping() {
			logWorker("replace", "stop router swap replace job", "toChainID", toChainID)
			return
		}
		restInJob(restIntervalInReplaceSwapJob)
	}
}

func findRouterSwapResultToReplace(toChainID string) ([]*mongodb.MgoSwapResult, error) {
	septime := getSepTimeInFind(maxReplaceSwapLifetime)
	return mongodb.FindRouterSwapResultsToReplace(toChainID, septime)
}

func processRouterSwapReplace(res *mongodb.MgoSwapResult) error {
	waitTimeToReplace := serverCfg.WaitTimeToReplace
	maxReplaceCount := serverCfg.MaxReplaceCount
	if waitTimeToReplace == 0 {
		waitTimeToReplace = defWaitTimeToReplace
	}
	if maxReplaceCount == 0 {
		maxReplaceCount = defMaxReplaceCount
	}
	if len(res.OldSwapTxs) > maxReplaceCount {
		checkAndRecycleSwapNonce(res)
		return nil
	}
	if res.SwapTx != "" && getSepTimeInFind(waitTimeToReplace) < res.Timestamp {
		return nil
	}
	return ReplaceRouterSwap(res, nil, false)
}

func checkAndRecycleSwapNonce(res *mongodb.MgoSwapResult) {
	if !params.IsParallelSwapEnabled() {
		return
	}
	_, err := verifyReplaceSwap(res, false)
	if err != nil {
		return
	}
	resBridge := router.GetBridgeByChainID(res.ToChainID)
	if resBridge == nil {
		return
	}
	nonceSetter, ok := resBridge.(tokens.NonceSetter)
	if !ok {
		return
	}
	if res.SwapNonce == 0 || res.MPC == "" {
		return
	}
	logWorker("recycle swap nonce", "swap", res)
	nonceSetter.RecycleSwapNonce(res.MPC, res.SwapNonce)
}

// ReplaceRouterSwap api
func ReplaceRouterSwap(res *mongodb.MgoSwapResult, gasPrice *big.Int, isManual bool) error {
	swap, err := verifyReplaceSwap(res, isManual)
	if err != nil {
		return err
	}

	resBridge := router.GetBridgeByChainID(res.ToChainID)
	if resBridge == nil {
		return tokens.ErrNoBridgeForChainID
	}
	routerMPC, err := router.GetRouterMPC(swap.GetTokenID(), res.ToChainID)
	if err != nil {
		return err
	}
	if !common.IsEqualIgnoreCase(res.MPC, routerMPC) {
		return tokens.ErrSenderMismatch
	}

	biFromChainID, biToChainID, biValue, err := getFromToChainIDAndValue(res.FromChainID, res.ToChainID, res.Value)
	if err != nil {
		return err
	}

	logWorker("replaceSwap", "process task", "swap", res)
	_ = updateSwapTimestamp(res.FromChainID, res.TxID, res.LogIndex)

	txid := res.TxID
	nonce := res.SwapNonce
	replaceNum := uint64(len(res.OldSwapTxs))
	if replaceNum == 0 {
		replaceNum++
	}
	args := &tokens.BuildTxArgs{
		SwapArgs: tokens.SwapArgs{
			Identifier:  params.GetIdentifier(),
			SwapID:      txid,
			SwapType:    tokens.SwapType(res.SwapType),
			Bind:        res.Bind,
			LogIndex:    res.LogIndex,
			FromChainID: biFromChainID,
			ToChainID:   biToChainID,
		},
		From:        res.MPC,
		OriginFrom:  swap.From,
		OriginTxTo:  swap.TxTo,
		OriginValue: biValue,
		Extra: &tokens.AllExtras{
			EthExtra: &tokens.EthExtraArgs{
				GasPrice: gasPrice,
				Nonce:    &nonce,
			},
			Sequence:   &nonce,
			ReplaceNum: replaceNum,
		},
	}
	args.SwapInfo, err = mongodb.ConvertFromSwapInfo(&swap.SwapInfo)
	if err != nil {
		return err
	}
	rawTx, err := resBridge.BuildRawTransaction(args)
	if err != nil {
		logWorkerError("replaceSwap", "build tx failed", err, "chainID", res.ToChainID, "txid", txid, "logIndex", res.LogIndex)
		return err
	}
	go signAndSendReplaceTx(resBridge, rawTx, args, res)
	return nil
}

func signAndSendReplaceTx(resBridge tokens.IBridge, rawTx interface{}, args *tokens.BuildTxArgs, res *mongodb.MgoSwapResult) {
	signedTx, txHash, err := resBridge.MPCSignTransaction(rawTx, args)
	if err != nil {
		logWorkerError("replaceSwap", "mpc sign tx failed", err, "fromChainID", res.FromChainID, "toChainID", res.ToChainID, "txid", res.TxID, "nonce", res.SwapNonce, "logIndex", res.LogIndex)
		if errors.Is(err, mpc.ErrGetSignStatusHasDisagree) {
			reverifySwap(args)
		}
		return
	}

	fromChainID := res.FromChainID
	txid := res.TxID
	logIndex := res.LogIndex

	err = mongodb.UpdateRouterOldSwapTxs(fromChainID, txid, logIndex, txHash)
	if err != nil {
		return
	}

	sentTxHash, err := sendSignedTransaction(resBridge, signedTx, args)
	if err == nil && txHash != sentTxHash {
		logWorkerError("replaceSwap", "send tx success but with different hash", errSendTxWithDiffHash,
			"fromChainID", fromChainID, "toChainID", res.ToChainID, "txid", txid, "nonce", res.SwapNonce,
			"logIndex", logIndex, "txHash", txHash, "sentTxHash", sentTxHash)
		_ = mongodb.UpdateRouterOldSwapTxs(fromChainID, txid, logIndex, sentTxHash)
	}
}

func verifyReplaceSwap(res *mongodb.MgoSwapResult, isManual bool) (*mongodb.MgoSwap, error) {
	fromChainID, txid, logIndex := res.FromChainID, res.TxID, res.LogIndex
	swap, err := mongodb.FindRouterSwap(fromChainID, txid, logIndex)
	if err != nil {
		return nil, err
	}
	if res.SwapTx == "" && !params.IsParallelSwapEnabled() {
		return nil, errors.New("swap without swaptx")
	}
	if res.SwapNonce == 0 && !isManual {
		return nil, errors.New("swap nonce is zero")
	}
	if res.Status != mongodb.MatchTxNotStable {
		return nil, errors.New("swap result status is not 'MatchTxNotStable'")
	}
	if res.SwapHeight != 0 && !isManual {
		return nil, errors.New("swaptx with block height")
	}
	resBridge := router.GetBridgeByChainID(res.ToChainID)
	if resBridge == nil {
		return nil, tokens.ErrNoBridgeForChainID
	}
	err = checkIfSwapNonceHasPassed(resBridge, res, true)
	if err != nil {
		return nil, err
	}
	return swap, nil
}

//nolint:gocyclo // ok
func checkIfSwapNonceHasPassed(bridge tokens.IBridge, res *mongodb.MgoSwapResult, isReplace bool) error {
	nonceSetter, ok := bridge.(tokens.NonceSetter)
	if !ok {
		return nil
	}
	nonce, err := nonceSetter.GetPoolNonce(res.MPC, "latest")
	if err != nil {
		return fmt.Errorf("get router mpc nonce failed, %w", err)
	}
	txStat := getSwapTxStatus(bridge, res)
	if txStat != nil && txStat.BlockHeight > 0 {
		if isReplace {
			return errors.New("swaptx exist in chain")
		}
		return nil
	}
	if nonce > res.SwapNonce && res.SwapNonce > 0 {
		var iden string
		if isReplace {
			iden = "[replace]"
		} else {
			iden = "[stable]"
		}
		fromChainID, txid, logIndex := res.FromChainID, res.TxID, res.LogIndex
		noncePassedInterval := params.GetNoncePassedConfirmInterval(res.ToChainID)
		if noncePassedInterval == 0 {
			noncePassedInterval = treatAsNoncePassedInterval
		}
		if res.Timestamp < getSepTimeInFind(noncePassedInterval) {
			if txStat == nil { // retry to get swap status
				txStat = getSwapTxStatus(bridge, res)
				if txStat != nil && txStat.BlockHeight > 0 {
					if isReplace {
						return errors.New("swaptx exist in chain")
					}
					return nil
				}
			}
			oldRes, errf := mongodb.FindRouterSwapResult(fromChainID, txid, logIndex)
			if errf != nil {
				return errf
			}
			if oldRes.Status == mongodb.Reswapping {
				return errors.New("forbid mark reswaping result to failed status")
			}
			logWorker(iden, "mark swap result nonce passed",
				"fromChainID", fromChainID, "txid", txid, "logIndex", logIndex,
				"swaptime", res.Timestamp, "nowtime", now())
			_ = markSwapResultFailed(fromChainID, txid, logIndex)
		}
		if isReplace {
			return fmt.Errorf("swap nonce (%v) is lower than latest nonce (%v)", res.SwapNonce, nonce)
		}
	}
	return nil
}
