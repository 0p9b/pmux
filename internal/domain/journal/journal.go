package journal

import "time"

type TxID string
type Step struct { Name string `json:"name"`; Action string `json:"action"`; Target string `json:"target,omitempty"`; Undo map[string]string `json:"undo,omitempty"`; At time.Time `json:"at"` }
type Tx struct { ID TxID `json:"id"`; Operation string `json:"operation"`; Metadata map[string]string `json:"metadata,omitempty"`; Steps []Step `json:"steps"`; State string `json:"state"`; BeganAt time.Time `json:"began_at"` }
type Journal interface { Begin(string, map[string]string) (TxID, error); Record(TxID, Step) error; Commit(TxID) error; Pending() ([]Tx, error); Rollback(TxID) error }
