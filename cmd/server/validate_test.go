package main

import (
	"testing"

	"github.com/aturzone/TONpayment/internal/config"
)

func TestValidate(t *testing.T) {
	t.Setenv("TON_ALLOW_FILE_STORE", "") // isolate from the ambient env

	// dev is permissive.
	if err := validate(&config.Config{Env: "dev"}); err != nil {
		t.Fatalf("dev should pass: %v", err)
	}

	// prod with neither a default address nor a create key = open minter -> fail.
	if err := validate(&config.Config{Env: "prod", DatabaseURL: "x"}); err == nil {
		t.Fatal("open-minter prod config should fail")
	}

	// prod with a key but no durable store and no opt-in -> fail.
	if err := validate(&config.Config{Env: "prod", CreateAPIKey: "k"}); err == nil {
		t.Fatal("non-durable prod store should fail")
	}

	// prod with the explicit file-store opt-in -> ok.
	t.Setenv("TON_ALLOW_FILE_STORE", "1")
	if err := validate(&config.Config{Env: "prod", CreateAPIKey: "k"}); err != nil {
		t.Fatalf("file-store opt-in should pass: %v", err)
	}
	t.Setenv("TON_ALLOW_FILE_STORE", "")

	// prod webhook without a secret -> fail.
	if err := validate(&config.Config{Env: "prod", TONReceiving: "UQx", DatabaseURL: "x", WebhookURL: "https://h"}); err == nil {
		t.Fatal("unsigned prod webhook should fail")
	}
	// prod webhook over http -> fail.
	if err := validate(&config.Config{Env: "prod", TONReceiving: "UQx", DatabaseURL: "x", WebhookURL: "http://h", WebhookSecret: "s"}); err == nil {
		t.Fatal("http prod webhook should fail")
	}
	// fully valid prod config -> ok.
	if err := validate(&config.Config{Env: "prod", TONReceiving: "UQx", DatabaseURL: "x", WebhookURL: "https://h", WebhookSecret: "s"}); err != nil {
		t.Fatalf("valid prod config should pass: %v", err)
	}
}
