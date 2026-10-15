package server

import "github.com/ariesworx/aries/internal/store"

// realStore returns the store on Dolt behind st, for a test that reads
// what the server never asks of a store, such as the event log. It
// panics on a fake, which no such test has.
func realStore(st Store) *store.Store { return st.(*store.Store) }
