package equilibrium

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"time"

	events "github.com/ChainSafe/chainbridge-substrate-events"
	"github.com/ChainSafe/chainbridge-utils/msg"
	"github.com/ethereum/go-ethereum/core/types"
	"gopkg.in/Graylog2/go-gelf.v2/gelf"
)

const loggerPrefix = "Equilibrium Bridge (Relay)"

var logger *Logger

type Logger struct {
	gelf        *gelf.TCPWriter
	environment string
}

func CreateGrayLogger(addr, environment string) error {
	if logger != nil {
		return fmt.Errorf("equilibrium logger already created")
	}

	gelfWriter, err := gelf.NewTCPWriter(addr)
	if err != nil {
		return err
	}
	gelfWriter.MaxReconnect = 10 // default: 3

	logger = &Logger{gelfWriter, environment}

	return nil
}

// type Message struct {
//  	Source       ChainId      // Source where message was initiated
//  	Destination  ChainId      // Destination chain of message
//  	Type         TransferType // type of bridge transfer
//  	DepositNonce Nonce        // Nonce for the deposit
//      ResourceId   ResourceId
//      Payload      []interface{} // data associated with event sequence
// }
//
// type Transaction struct {
//      func Hash() common.Hash
//      func Value() *big.Int
//      func To() *common.Address
// }
func Message(action, text string, m msg.Message, tx *types.Transaction, data []byte) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}

	ctx := make([]interface{}, 0)
	ctx = append(ctx, "source_chain", m.Source)
	ctx = append(ctx, "destination_chain", m.Destination)
	ctx = append(ctx, "action", action)
	ctx = append(ctx, "nonce", m.DepositNonce)
	ctx = append(ctx, "type", m.Type)
	ctx = append(ctx, "resource_id", hex.EncodeToString(m.ResourceId[:]))

	if m.Type == msg.FungibleTransfer {
		if len(m.Payload) > 0 {
			valueBytes, ok := m.Payload[0].([]byte)
			if ok {
				var value big.Int
				value.SetBytes(valueBytes)
				ctx = append(ctx, "value", value.String())
			} else {
				ctx = append(ctx, "value", fmt.Sprintf("%v", m.Payload[0]))
			}
		}
		if len(m.Payload) > 1 {
			recipient, ok := m.Payload[1].([]byte)
			if ok {
				ctx = append(ctx, "recipient", hex.EncodeToString(recipient))
			} else {
				ctx = append(ctx, "recipient", fmt.Sprintf("%v", m.Payload[1]))
			}
		}
	}

	if tx != nil {
		// js, err := tx.MarshalJSON()
		// if err != nil {
		// 	ctx = append(ctx, "tx_json", err.Error())
		// } else {
		// 	ctx = append(ctx, "tx_json", string(js))
		// }

		ctx = append(ctx, "tx_hash", tx.Hash().Hex())
	}

	if data != nil {
		value := hex.EncodeToString(data)
		if action != "AcknowledgeProposal" {
			ctx = append(ctx, "data_hash", value)
		} else {
			ctx = append(ctx, "proposal", formatProposal(value))
		}
	}

	Info(text, ctx...)
}

// type EventFungibleTransfer struct {
//  	Destination  types.U8
//  	DepositNonce types.U64
//      ResourceId   types.Bytes32
//  	Amount       types.U256
//  	Recipient    types.Bytes
// }
func EventFungibleTransfer(action, text string, e events.EventFungibleTransfer) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}
	ctx := make([]interface{}, 0)
	ctx = append(ctx, "action", action)
	ctx = append(ctx, "destination_chain", e.Destination)
	ctx = append(ctx, "nonce", e.DepositNonce)
	ctx = append(ctx, "resource_id", hex.EncodeToString(e.ResourceId[:]))
	ctx = append(ctx, "value", e.Amount.Int.String())
	ctx = append(ctx, "recipient", hex.EncodeToString(e.Recipient))
	Info(text, ctx...)
}

func Info(text string, ctx ...interface{}) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}
	message := newMessage(text, ctx...)
	message.Level = gelf.LOG_INFO
	_, _ = fmt.Fprintf(os.Stdout, "==== Graylog: %v\n", message)
	err := logger.gelf.WriteMessage(message)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "WriteMessage error: %s\n", err.Error())
	}
}

func Warn(text string, ctx ...interface{}) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}
	message := newMessage(text, ctx...)
	message.Level = gelf.LOG_WARNING
	err := logger.gelf.WriteMessage(message)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "WriteMessage error: %s\n", err.Error())
	}
}

func Error(text string, err error, ctx ...interface{}) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}
	message := newMessage(text+": "+err.Error(), ctx...)
	message.Level = gelf.LOG_ERR
	err = logger.gelf.WriteMessage(message)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "WriteMessage error: %s\n", err.Error())
	}
}

func newMessage(text string, ctx ...interface{}) *gelf.Message {
	hostname, err := os.Hostname()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Hostname error: %s\n", err.Error())
	}

	attrs := newAttributes(ctx...)

	return &gelf.Message{
		Version:  "1.1",
		Host:     hostname,
		Short:    loggerPrefix + " " + text,
		Full:     text,
		TimeUnix: float64(time.Now().UnixNano()) / float64(time.Second),
		Level:    gelf.LOG_INFO,
		Facility: "Equilibrium",
		Extra:    attrs,
	}
}

func newAttributes(ctx ...interface{}) map[string]interface{} {
	attrs := map[string]interface{}{
		"environment": logger.environment,
	}

	N := len(ctx)
	if N%2 != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "Uneven set of attributes '%v'\n", ctx)
		return attrs
	}

	for i := 0; i < N-1; i += 2 {
		name := fmt.Sprintf("%v", ctx[i])
		value := ctx[i+1]
		attrs[name] = value
	}

	return attrs
}

/// Splits string in three substrings to represent hashes of proposal.
/// s0: hash of prefix+method
/// s1: hash of srcId
/// s2: hash of proposal itself
/// See also ChainBridge.votes method.
func formatProposal(s string) string {
	const chunkLen = 64
	var s0, s1, s2 string
	for i, c := range s {
		if i < chunkLen {
			s0 += string(c)
		} else if i < 2*chunkLen {
			s1 += string(c)
		} else {
			s2 += string(c)
		}
	}
	return "[0x" + s0 + ", 0x" + s1 + ", 0x" + s2 + "]"
}
