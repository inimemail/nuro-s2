package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestAccountCellDirectoryFreezesExistingAssignments(t *testing.T) {
	mr := miniredis.RunT(t)
	directory := NewAccountCellDirectory(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	first, err := directory.EnsureAssignment(ctx, 11, []string{"cell-a", "cell-b"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := directory.EnsureAssignment(ctx, 11, []string{"cell-a", "cell-b", "cell-c"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("existing account moved from %q to %q", first, second)
	}
}

func TestAccountCellDirectoryRegistrationIsImmutableAndPlatformScoped(t *testing.T) {
	mr := miniredis.RunT(t)
	directory := NewAccountCellDirectory(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	if err := directory.RegisterCell(ctx, "openai", "cell-a", "redis://one:6379/0"); err != nil {
		t.Fatal(err)
	}
	if err := directory.RegisterCell(ctx, "openai", "cell-a", "redis://one:6379/0"); err != nil {
		t.Fatalf("idempotent registration failed: %v", err)
	}
	if err := directory.RegisterCell(ctx, "openai", "cell-a", "redis://two:6379/0"); !errors.Is(err, ErrAccountCellEndpointConflict) {
		t.Fatalf("endpoint overwrite was not rejected: %v", err)
	}
	if err := directory.RegisterCell(ctx, "anthropic", "cell-a", "redis://one:6379/0"); !errors.Is(err, ErrAccountCellPlatformConflict) {
		t.Fatalf("cross-platform registration was not rejected: %v", err)
	}
	endpoint, err := directory.Endpoint(ctx, "cell-a")
	if err != nil || endpoint != "redis://one:6379/0" {
		t.Fatalf("immutable endpoint changed: endpoint=%q err=%v", endpoint, err)
	}
	belongs, err := directory.CellBelongsTo(ctx, "cell-a", "anthropic")
	if err != nil || belongs {
		t.Fatalf("Cell leaked into another platform: belongs=%v err=%v", belongs, err)
	}
}

func TestAccountCellDirectoryRegistrationRejectsLegacyCrossPlatformEntry(t *testing.T) {
	mr := miniredis.RunT(t)
	directory := NewAccountCellDirectory(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	if err := directory.rdb.SAdd(ctx, accountCellPlatformCatalog+"anthropic", "legacy-cell").Err(); err != nil {
		t.Fatal(err)
	}

	err := directory.RegisterCell(ctx, "openai", "legacy-cell", "redis://one:6379/0")
	if !errors.Is(err, ErrAccountCellPlatformConflict) {
		t.Fatalf("legacy cross-platform registration was not rejected: %v", err)
	}
	belongs, lookupErr := directory.CellBelongsTo(ctx, "legacy-cell", "openai")
	if lookupErr != nil || belongs {
		t.Fatalf("legacy Cell leaked into target platform: belongs=%v err=%v", belongs, lookupErr)
	}
}

func TestAccountCellDirectoryCatalogOnlyAffectsNewAccounts(t *testing.T) {
	mr := miniredis.RunT(t)
	directory := NewAccountCellDirectory(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	if err := directory.RegisterForNewAccounts(ctx, "cell-a"); err != nil {
		t.Fatal(err)
	}
	first, err := directory.EnsureAssignment(ctx, 21, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.RegisterForNewAccounts(ctx, "cell-b"); err != nil {
		t.Fatal(err)
	}
	if err := directory.rdb.Del(ctx, accountCellCatalogKey).Err(); err != nil {
		t.Fatal(err)
	}
	again, err := directory.EnsureAssignment(ctx, 21, nil)
	if err != nil || again != first {
		t.Fatalf("catalog growth moved existing account: before=%q after=%q err=%v", first, again, err)
	}
}

func TestAccountCellDirectoryMigrationFailsClosedWithoutOwnerCellFence(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	directory := NewAccountCellDirectory(client)
	ctx := context.Background()
	if _, err := directory.EnsureAssignment(ctx, 12, []string{"cell-a", "cell-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.BeginMigration(ctx, 12, "cell-b", 1); err != ErrAccountCellBusy {
		t.Fatalf("active migration was not rejected: %v", err)
	}
	if _, err := directory.BeginMigration(ctx, 12, "cell-b", 0); err != ErrAccountCellMigrationDisabled {
		t.Fatalf("unfenced migration was not rejected: %v", err)
	}
	if err := directory.CommitMigration(ctx, 12, "cell-b", 1, 0); err != ErrAccountCellMigrationDisabled {
		t.Fatalf("unfenced migration commit was not rejected: %v", err)
	}
	cell, err := directory.Cell(ctx, 12)
	if err != nil || cell == "cell-b" {
		t.Fatalf("disabled migration changed ownership: cell=%q err=%v", cell, err)
	}
}
