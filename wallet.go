package ethereum

import (
	"encoding/hex"
	"fmt"

	"github.com/umbracle/ethgo/wallet"
	"go.k6.io/k6/js/modules"
)

// Wallet struct for JavaScript binding
type Wallet struct{}

// Key struct to hold private key and address
type Key struct {
	PrivateKey string
	Address    string
}

func init() {
	wallet := Wallet{}
	modules.Register("k6/x/ethereum/wallet", &wallet)
}

// GenerateKey creates a random key
func (w *Wallet) GenerateKey() (*Key, error) {
	k, err := wallet.GenerateKey()
	if err != nil {
		return nil, err
	}
	pk, err := k.MarshallPrivateKey()
	if err != nil {
		return nil, err
	}
	pks := hex.EncodeToString(pk)

	return &Key{
		PrivateKey: pks,
		Address:    k.Address().String(),
	}, err
}

// **AddressFromPrivateKey** function to get the address from a private key
func (w *Wallet) AddressFromPrivateKey(privateKey string) (string, error) {
	// Convert private key from hex string to wallet key
	privBytes, err := hex.DecodeString(privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to decode private key: %w", err)
	}

	walletKey, err := wallet.NewWalletFromPrivKey(privBytes)
	if err != nil {
		return "", fmt.Errorf("failed to create wallet from private key: %w", err)
	}

	return walletKey.Address().String(), nil
}
