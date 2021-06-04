// Copyright 2020 ChainSafe Systems
// SPDX-License-Identifier: LGPL-3.0-only

package ethereum

import (
	"context"
	"errors"
	"fmt"
	erc20 "github.com/ChainSafe/ChainBridge/bindings/ERC20"
	"github.com/ChainSafe/chainbridge-utils/core"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"math/big"
	"time"

	"github.com/ChainSafe/chainbridge-utils/msg"
	log "github.com/ChainSafe/log15"

	"github.com/ChainSafe/ChainBridge/shared/equilibrium"
	utils "github.com/ChainSafe/ChainBridge/shared/ethereum"
)

// Number of blocks to wait for an finalization event
const ExecuteBlockWatchLimit = 100

// Time between retrying a failed tx
const TxRetryInterval = time.Second * 2

// Maximum number of tx retries before exiting
const TxRetryLimit = 10

var ErrNonceTooLow = errors.New("nonce too low")
var ErrTxUnderpriced = errors.New("replacement transaction underpriced")
var ErrFatalTx = errors.New("submission of transaction failed")
var ErrFatalQuery = errors.New("query of chain state failed")

// proposalIsComplete returns true if the proposal state is either Passed, Transferred or Cancelled
func (w *writer) proposalIsComplete(srcId msg.ChainId, nonce msg.Nonce, dataHash [32]byte, messageContext core.MessageContext) bool {
	prop, err := w.bridgeContract.GetProposal(w.conn.CallOpts(), uint8(srcId), uint64(nonce), dataHash)
	if err != nil {
		w.log.ErrorTransfer("Failed to check proposal existence", messageContext, "err", err)
		return false
	}
	return prop.Status == PassedStatus || prop.Status == TransferredStatus || prop.Status == CancelledStatus
}

// proposalIsComplete returns true if the proposal state is Transferred or Cancelled
func (w *writer) proposalIsFinalized(srcId msg.ChainId, nonce msg.Nonce, dataHash [32]byte, messageContext core.MessageContext) bool {
	prop, err := w.bridgeContract.GetProposal(w.conn.CallOpts(), uint8(srcId), uint64(nonce), dataHash)
	if err != nil {
		w.log.ErrorTransfer("Failed to check proposal existence", messageContext, "err", err)
		return false
	}
	return prop.Status == TransferredStatus || prop.Status == CancelledStatus // Transferred (3)
}

// hasVoted checks if this relayer has already voted
func (w *writer) hasVoted(srcId msg.ChainId, nonce msg.Nonce, dataHash [32]byte, messageContext core.MessageContext) bool {
	hasVoted, err := w.bridgeContract.HasVotedOnProposal(w.conn.CallOpts(), utils.IDAndNonce(srcId, nonce), dataHash, w.conn.Opts().From)
	if err != nil {
		w.log.ErrorTransfer("Failed to check proposal existence", messageContext, "err", err)
		return false
	}

	return hasVoted
}

func (w *writer) shouldVote(m msg.Message, dataHash [32]byte, messageContext core.MessageContext) bool {
	direction := "S->E"
	action := "Info"

	// Check if proposal has passed and skip if Passed or Transferred
	if w.proposalIsComplete(m.Source, m.DepositNonce, dataHash, messageContext) {
		w.log.Info("Proposal complete, not voting", "src", m.Source, "nonce", m.DepositNonce)
		equilibrium.Message(action, fmt.Sprintf("(%s) Proposal complete, not voting", direction),
			m, nil, dataHash[:], messageContext)
		return false
	}

	// Check if relayer has previously voted
	if w.hasVoted(m.Source, m.DepositNonce, dataHash, messageContext) {
		w.log.Info("Relayer has already voted, not voting", "src", m.Source, "nonce", m.DepositNonce)
		equilibrium.Message(action, fmt.Sprintf("(%s) Relayer has already voted, not voting", direction),
			m, nil, dataHash[:], messageContext)
		return false
	}

	return true
}

// createErc20Proposal creates an Erc20 proposal.
// Returns true if the proposal is successfully created or is complete
func (w *writer) createErc20Proposal(m msg.Message, messageContext core.MessageContext) bool {
	w.log.Info("Creating erc20 proposal", "src", m.Source, "nonce", m.DepositNonce)

	resourceId := m.ResourceId

	tokenAddress, err := w.erc20HandlerContract.ResourceIDToTokenContractAddress(&bind.CallOpts{From: w.conn.Keypair().CommonAddress()}, resourceId)
	if err != nil {
		w.log.ErrorTransfer("Unable to get contract address by resourceId", messageContext, "err", err)
		return false
	}

	erc20Contract, err := erc20.NewERC20(tokenAddress, w.conn.Client())
	if err != nil {
		w.log.ErrorTransfer("Error Creating ERC20 Contract From TokenAddress", messageContext, "err", err)
		return false
	}

	tokenDecimals, err := erc20Contract.Decimals(&bind.CallOpts{From: w.conn.Keypair().CommonAddress()})
	if err != nil {
		w.log.ErrorTransfer("Error Getting Token Decimals", messageContext)
		return false
	}

	amount := new(big.Int).SetBytes(m.Payload[0].([]byte))

	if tokenDecimals <= 18 {
		divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(18 - int64(tokenDecimals)), nil)
		amount = new(big.Int).Div(amount, divisor)
	} else {
		multiplier := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(tokenDecimals) - 18), nil)
		amount = new(big.Int).Mul(amount, multiplier)
	}

	data := ConstructErc20ProposalData(amount.Bytes(), m.Payload[1].([]byte))
	dataHash := utils.Hash(append(w.cfg.erc20HandlerContract.Bytes(), data...))

	if !w.shouldVote(m, dataHash, messageContext) {
		return false
	}

	// Capture latest block so when know where to watch from
	latestBlock, err := w.conn.LatestBlock()
	if err != nil {
		w.log.ErrorTransfer("Unable to fetch latest block", messageContext, "err", err)
		return false
	}

	// watch for execution event
	go w.watchThenExecute(m, data, dataHash, latestBlock, messageContext)

	w.voteProposal(m, dataHash, messageContext)

	return true
}

// createErc721Proposal creates an Erc721 proposal.
// Returns true if the proposal is succesfully created or is complete
func (w *writer) createErc721Proposal(m msg.Message) bool {
	w.log.Info("Creating erc721 proposal", "src", m.Source, "nonce", m.DepositNonce)

	data := ConstructErc721ProposalData(m.Payload[0].([]byte), m.Payload[1].([]byte), m.Payload[2].([]byte))
	dataHash := utils.Hash(append(w.cfg.erc721HandlerContract.Bytes(), data...))

	if !w.shouldVote(m, dataHash, make(core.MessageContext)) {
		return false
	}

	// Capture latest block so we know where to watch from
	latestBlock, err := w.conn.LatestBlock()
	if err != nil {
		w.log.Error("Unable to fetch latest block", "err", err)
		return false
	}

	// watch for execution event
	go w.watchThenExecute(m, data, dataHash, latestBlock, make(core.MessageContext))

	w.voteProposal(m, dataHash, make(core.MessageContext))

	return true
}

// createGenericDepositProposal creates a generic proposal
// returns true if the proposal is complete or is succesfully created
func (w *writer) createGenericDepositProposal(m msg.Message) bool {
	w.log.Info("Creating generic proposal", "src", m.Source, "nonce", m.DepositNonce)

	metadata := m.Payload[0].([]byte)
	data := ConstructGenericProposalData(metadata)
	toHash := append(w.cfg.genericHandlerContract.Bytes(), data...)
	dataHash := utils.Hash(toHash)

	if !w.shouldVote(m, dataHash, make(core.MessageContext)) {
		return false
	}

	// Capture latest block so when know where to watch from
	latestBlock, err := w.conn.LatestBlock()
	if err != nil {
		w.log.Error("Unable to fetch latest block", "err", err)
		return false
	}

	// watch for execution event
	go w.watchThenExecute(m, data, dataHash, latestBlock, make(core.MessageContext))

	w.voteProposal(m, dataHash, make(core.MessageContext))

	return true
}

// watchThenExecute watches for the latest block and executes once the matching finalized event is found
func (w *writer) watchThenExecute(m msg.Message, data []byte, dataHash [32]byte, latestBlock *big.Int, messageContext core.MessageContext) {
	w.log.Info("Watching for finalization event", "src", m.Source, "nonce", m.DepositNonce)

	// watching for the latest block, querying and matching the finalized event will be retried up to ExecuteBlockWatchLimit times
	for i := 0; i < ExecuteBlockWatchLimit; i++ {
		select {
		case <-w.stop:
			return
		default:
			// watch for the lastest block, retry up to BlockRetryLimit times
			for waitRetrys := 0; waitRetrys < BlockRetryLimit; waitRetrys++ {
				err := w.conn.WaitForBlock(latestBlock)
				if err != nil {
					w.log.ErrorTransfer("Waiting for block failed", messageContext, "err", err)
					// Exit if retries exceeded
					if waitRetrys+1 == BlockRetryLimit {
						w.log.ErrorTransfer("Waiting for block retries exceeded, shutting down", messageContext)
						w.sysErr <- ErrFatalQuery
						return
					}
				} else {
					break
				}
			}

			// query for logs
			query := buildQuery(w.cfg.bridgeContract, utils.ProposalEvent, latestBlock, latestBlock)
			evts, err := w.conn.Client().FilterLogs(context.Background(), query)
			if err != nil {
				w.log.ErrorTransfer("Failed to fetch logs", messageContext, "err", err)
				return
			}

			// execute the proposal once we find the matching finalized event
			for _, evt := range evts {
				sourceId := evt.Topics[1].Big().Uint64()
				depositNonce := evt.Topics[2].Big().Uint64()
				status := evt.Topics[3].Big().Uint64()

				if m.Source == msg.ChainId(sourceId) &&
					m.DepositNonce.Big().Uint64() == depositNonce &&
					utils.IsFinalized(uint8(status)) {
					w.executeProposal(m, data, dataHash, messageContext)
					return
				} else {
					w.log.Trace("Ignoring event", "src", sourceId, "nonce", depositNonce)
				}
			}
			w.log.Trace("No finalization event found in current block", "block", latestBlock, "src", m.Source, "nonce", m.DepositNonce)
			latestBlock = latestBlock.Add(latestBlock, big.NewInt(1))
		}
	}
	log.Warn("Block watch limit exceeded, skipping execution", "source", m.Source, "dest", m.Destination, "nonce", m.DepositNonce)
}

// voteProposal submits a vote proposal
// a vote proposal will try to be submitted up to the TxRetryLimit times
func (w *writer) voteProposal(m msg.Message, dataHash [32]byte, messageContext core.MessageContext) {
	direction := "S->E"
	for i := 0; i < TxRetryLimit; i++ {
		select {
		case <-w.stop:
			return
		default:
			err := w.conn.LockAndUpdateOpts()
			if err != nil {
				w.log.ErrorTransfer("Failed to update tx opts", messageContext, "err", err)
				continue
			}

			tx, err := w.bridgeContract.VoteProposal(
				w.conn.Opts(),
				uint8(m.Source),
				uint64(m.DepositNonce),
				m.ResourceId,
				dataHash,
			)
			w.conn.UnlockOpts()

			if err == nil {
				w.log.Info("Submitted proposal vote", "tx", tx.Hash(), "src", m.Source, "depositNonce", m.DepositNonce)
				equilibrium.Message("ProposalVote", fmt.Sprintf("(%s) SubmitTx ProposalVote", direction),
					m, tx, dataHash[:], messageContext)
				if w.metrics != nil {
					w.metrics.VotesSubmitted.Inc()
				}
				return
			} else if err.Error() == ErrNonceTooLow.Error() || err.Error() == ErrTxUnderpriced.Error() {
				w.log.Debug("Nonce too low, will retry")
				time.Sleep(TxRetryInterval)
			} else {
				w.log.Warn("Voting failed", "source", m.Source, "dest", m.Destination, "depositNonce", m.DepositNonce, "err", err)
				time.Sleep(TxRetryInterval)
			}

			// Verify proposal is still open for voting, otherwise no need to retry
			if w.proposalIsComplete(m.Source, m.DepositNonce, dataHash, messageContext) {
				w.log.Info("Proposal voting complete on chain", "src", m.Source, "dst", m.Destination, "nonce", m.DepositNonce)
				equilibrium.Message("Info", fmt.Sprintf("(%s) Proposal voting complete on chain", direction),
					m, tx, dataHash[:], messageContext)
				return
			}
		}
	}
	w.log.ErrorTransfer("Submission of Vote transaction failed", messageContext, "source", m.Source, "dest", m.Destination, "depositNonce", m.DepositNonce)
	w.sysErr <- ErrFatalTx
}

// executeProposal executes the proposal
func (w *writer) executeProposal(m msg.Message, data []byte, dataHash [32]byte, messageContext core.MessageContext) {
	direction := "S->E"
	for i := 0; i < TxRetryLimit; i++ {
		select {
		case <-w.stop:
			return
		default:
			err := w.conn.LockAndUpdateOpts()
			if err != nil {
				w.log.ErrorTransfer("Failed to update nonce", messageContext, "err", err)
				return
			}

			tx, err := w.bridgeContract.ExecuteProposal(
				w.conn.Opts(),
				uint8(m.Source),
				uint64(m.DepositNonce),
				data,
				m.ResourceId,
			)
			w.conn.UnlockOpts()

			if err == nil {
				w.log.Info("Submitted proposal execution", "tx", tx.Hash(), "src", m.Source, "dst", m.Destination, "nonce", m.DepositNonce)
				equilibrium.Message("ProposalExecute", fmt.Sprintf("(%s) SubmitTx ProposalExecute", direction),
					m, tx, dataHash[:], messageContext)
				return
			} else if err.Error() == ErrNonceTooLow.Error() || err.Error() == ErrTxUnderpriced.Error() {
				w.log.ErrorTransfer("Nonce too low, will retry", messageContext)
				time.Sleep(TxRetryInterval)
			} else {
				w.log.Warn("Execution failed, proposal may already be complete", "err", err)
				time.Sleep(TxRetryInterval)
			}

			// Verify proposal is still open for execution, tx will fail if we aren't the first to execute,
			// but there is no need to retry
			if w.proposalIsFinalized(m.Source, m.DepositNonce, dataHash, messageContext) {
				w.log.Info("Proposal finalized on chain", "src", m.Source, "dst", m.Destination, "nonce", m.DepositNonce)
				equilibrium.Message("Info", fmt.Sprintf("(%s) Proposal finalized on chain", direction),
					m, tx, dataHash[:], messageContext)
				return
			}
		}
	}
	w.log.ErrorTransfer("Submission of Execute transaction failed", messageContext, "source", m.Source, "dest", m.Destination, "depositNonce", m.DepositNonce)
	w.sysErr <- ErrFatalTx
}
