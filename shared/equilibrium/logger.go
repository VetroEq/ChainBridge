package equilibrium

import (
	"encoding/hex"
	"fmt"
	"github.com/ChainSafe/chainbridge-utils/core"
	"math/big"
	"os"
	"time"

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

func Message(action, text string, m msg.Message, tx *types.Transaction, data []byte, messageContext core.MessageContext) {
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
	ctx = append(ctx, "service", "bridge")

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

	info(text, messageContext, ctx...)
}

func writeToGelf(text string, messageContext core.MessageContext, logLevel int32, ctx ...interface{}) {
	if logger == nil {
		_, _ = fmt.Fprintf(os.Stderr, "Graylog writing is disabled")
		return
	}

	for name, value := range messageContext {
		ctx = append(ctx, name, value)
	}

	message := newMessage(text, ctx...)
	message.Level = logLevel
	_, _ = fmt.Fprintf(os.Stdout, "==== Graylog: %v\n", message)
	err := logger.gelf.WriteMessage(message)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "WriteMessage error: %s\n", err.Error())
	}
}

func info(text string, messageContext core.MessageContext, ctx ...interface{}) {
	writeToGelf(text, messageContext, gelf.LOG_INFO, ctx...)
}

func Error(text string, messageContext core.MessageContext, ctx ...interface{}) {
	writeToGelf(text, messageContext, gelf.LOG_ERR, ctx...)
}

func Crit(text string, messageContext core.MessageContext, ctx ...interface{}) {
	writeToGelf(text, messageContext, gelf.LOG_CRIT, ctx...)
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
