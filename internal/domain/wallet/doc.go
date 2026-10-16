// Package wallet holds the Wallet aggregate root and its immutable
// LedgerEntry. Every balance change goes through Debit or Credit, produces
// the matching entry and moves the version by exactly one.
package wallet
