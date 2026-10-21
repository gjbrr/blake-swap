package localwallet

import (
	"context"
	"fmt"

	"git.smesh.lol/tinywallet/pkg/domain"
	"git.smesh.lol/tinywallet/pkg/psbt"
)

// Direct answers the swap's wallet operations from a wallet running in this process.
//
// It is the native binding: the rig proof and a co-located CLI have the wallet and the swap in
// one program, so the channel is a function call. A browser build binds the same interface to a
// worker over the same methods, which is why nothing above this type knows which one it has.
type Direct struct {
	// Domain is the wallet.
	Domain *domain.Domain
}

// NewDirect wraps a wallet domain.
func NewDirect(dom *domain.Domain) *Direct { return &Direct{Domain: dom} }

// Height is the chain's verified tip, from the wallet's stored position.
func (d *Direct) Height(_ context.Context, chain string) (int64, error) {
	status, err := d.Domain.Status(chain)

	if err != nil {
		return 0, err
	}

	return int64(status.Position.Tip), nil
}

// Coins lists the chain's own money, post-fork only.
func (d *Direct) Coins(_ context.Context, chain string) ([]Coin, error) {
	coins, err := d.Domain.SpendableOn(chain)

	if err != nil {
		return nil, err
	}

	out := make([]Coin, 0, len(coins))

	for _, coin := range coins {
		out = append(out, Coin{
			Chain:  coin.Chain,
			TxID:   displayTxID(coin.TxID),
			Vout:   coin.Vout,
			Value:  coin.Value,
			Height: coin.Height,
			Script: coin.Script,
		})
	}

	return out, nil
}

// Address returns the next receive address and its public key.
func (d *Direct) Address(_ context.Context, chain string) (string, []byte, error) {
	status, err := d.Domain.Status(chain)

	if err != nil {
		return "", nil, err
	}

	addresses, err := d.Domain.Addresses(chain, false, status.Receive, 1)

	if err != nil {
		return "", nil, err
	}

	if len(addresses) != 1 {
		return "", nil, fmt.Errorf("localwallet: %d addresses", len(addresses))
	}

	return addresses[0].Text, addresses[0].Pubkey, nil
}

// Send pays an address from the wallet's coins.
func (d *Direct) Send(_ context.Context, chain, address string, sats, fee uint64, policy Policy) (string, error) {
	txid, err := d.Domain.Send(chain, address, sats, fee, domain.SendPolicy{
		MaxSpend: policy.MaxSpend,
		MaxFee:   policy.MaxFee,
	})

	if err != nil {
		return "", err
	}

	return displayTxID(txid), nil
}

// Derive builds a contract from its terms.
func (d *Direct) Derive(_ context.Context, chain string, terms HTLCTerms) (Contract, error) {
	address, witness, program, err := d.Domain.DeriveHTLC(
		chain, terms.Claimer, terms.Refunder, terms.SecretHash, terms.Locktime,
	)

	if err != nil {
		return Contract{}, err
	}

	return Contract{Address: address, Script: witness, Program: program[:]}, nil
}

// Watch adds a contract's output script to the chain's scan set.
func (d *Direct) Watch(_ context.Context, chain string, script []byte) error {
	return d.Domain.Watch(chain, script)
}

// SignPSBT signs the wallet's inputs, including its own contracts.
func (d *Direct) SignPSBT(_ context.Context, chain string, raw []byte, policy Policy) ([]byte, int, error) {
	return d.Domain.SignPSBT(chain, raw, psbt.Policy{MaxSpend: policy.MaxSpend, MaxFee: policy.MaxFee})
}

// RawTx is the bytes of a transaction the wallet sent.
func (d *Direct) RawTx(_ context.Context, chain, txid string) (string, error) {
	sent, err := d.Domain.SentTx(chain, txid)

	if err != nil {
		return "", err
	}

	return sent.Hex, nil
}

// DeriveHTLC answers the contract derivation the engine asks a node for, so a build with a
// local wallet needs no descriptor compiler.
func (d *Direct) DeriveHTLC(_ context.Context, ch string, terms HTLCTerms) (Contract, error) {
	return d.Derive(context.Background(), ch, terms)
}

// displayTxID renders a transaction id the way it is printed.
func displayTxID(txid [32]byte) string {
	reversed := make([]byte, len(txid))

	for i, b := range txid {
		reversed[len(txid)-1-i] = b
	}

	return fmt.Sprintf("%x", reversed)
}
