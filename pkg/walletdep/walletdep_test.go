package walletdep

import (
	"errors"
	"strings"
	"testing"
)

// TestTheRefusalNamesTheOperation: an operator must be able to see which call failed and why.
func TestTheRefusalNamesTheOperation(t *testing.T) {
	err := Refuse("createwallet")

	if !errors.Is(err, ErrNoWalletService) {
		t.Fatal("the refusal does not unwrap to the sentinel")
	}

	if !strings.HasPrefix(err.Error(), "createwallet: ") {
		t.Fatalf("the message is %q", err.Error())
	}

	if !strings.Contains(err.Error(), "tinywallet") {
		t.Fatal("the message does not say where the wallet is")
	}
}

// TestDifferentOperationsAreDifferentErrors: the sentinel is shared, the messages are not.
func TestDifferentOperationsAreDifferentErrors(t *testing.T) {
	first := Refuse("walletprocesspsbt")
	second := Refuse("importdescriptors")

	if first.Error() == second.Error() {
		t.Fatal("two operations render the same message")
	}

	if !errors.Is(first, ErrNoWalletService) || !errors.Is(second, ErrNoWalletService) {
		t.Fatal("a refusal does not unwrap to the sentinel")
	}
}
