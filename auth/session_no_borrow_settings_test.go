package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestSessionNoBorrowSettingsDefaultOffAndHotUpdate(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	if store.SessionNoBorrowEnabled() {
		t.Fatal("no-borrow must default off")
	}
	if got := store.SessionNoBorrowHold(); got != 20*time.Second {
		t.Fatalf("default hold = %s, want 20s", got)
	}
	store.SetSessionNoBorrow(true, 5*time.Second)
	if !store.SessionNoBorrowEnabled() || store.SessionNoBorrowHold() != 5*time.Second {
		t.Fatalf("hot update lost: enabled=%v hold=%s", store.SessionNoBorrowEnabled(), store.SessionNoBorrowHold())
	}
	store.SetSessionNoBorrow(true, 0)
	if got := store.SessionNoBorrowHold(); got != 20*time.Second {
		t.Fatalf("zero hold must normalize to default: %s", got)
	}
	store.SetSessionNoBorrow(true, 2*time.Minute)
	if got := store.SessionNoBorrowHold(); got != 30*time.Second {
		t.Fatalf("hold must cap at 30s: %s", got)
	}
}

func TestNewStoreLoadsSessionNoBorrowFromSystemSettings(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, CodexSessionNoBorrowEnabled: true, CodexSessionNoBorrowHoldSeconds: 12})
	if !store.SessionNoBorrowEnabled() || store.SessionNoBorrowHold() != 12*time.Second {
		t.Fatalf("NewStore did not load no-borrow settings: enabled=%v hold=%s", store.SessionNoBorrowEnabled(), store.SessionNoBorrowHold())
	}
}
