package surrealdb

import (
	"time"

	"github.com/surrealdb/surrealdb.go/pkg/models"
)

// Wire types for SurrealDB query results.

type entityRecord struct {
	ID   *models.RecordID `json:"id"`
	UUID string           `json:"uuid"`
}

type episodeRecord struct {
	ID *models.RecordID `json:"id"`
}

type episodeFullRecord struct {
	ID        *models.RecordID `json:"id"`
	UUID      string           `json:"uuid"`
	Name      string           `json:"name"`
	Body      string           `json:"body"`
	Source    string           `json:"source"`
	GroupID   string           `json:"group_id"`
	CreatedAt time.Time        `json:"created_at"`
}

type factRow struct {
	RUUID      string     `json:"r_uuid"`
	RType      string     `json:"r_type"`
	RFact      string     `json:"r_fact"`
	RCreatedAt time.Time  `json:"r_created_at"`
	RValidAt   time.Time  `json:"r_valid_at"`
	RInvalidAt *time.Time `json:"r_invalid_at"`
	SrcUUID    string     `json:"src_uuid"`
	SrcName    string     `json:"src_name"`
	SrcType    string     `json:"src_type"`
	SrcSummary string     `json:"src_summary"`
	TgtUUID    string     `json:"tgt_uuid"`
	TgtName    string     `json:"tgt_name"`
	TgtType    string     `json:"tgt_type"`
	TgtSummary string     `json:"tgt_summary"`
	Score      float64    `json:"score"`
}

type graphRow struct {
	AUUID      string     `json:"a_uuid"`
	AName      string     `json:"a_name"`
	AType      string     `json:"a_type"`
	ASummary   string     `json:"a_summary"`
	RUUID      string     `json:"r_uuid"`
	RType      string     `json:"r_type"`
	RFact      string     `json:"r_fact"`
	RCreatedAt time.Time  `json:"r_created_at"`
	RValidAt   time.Time  `json:"r_valid_at"`
	RInvalidAt *time.Time `json:"r_invalid_at"`
	BUUID      string     `json:"b_uuid"`
	BName      string     `json:"b_name"`
	BType      string     `json:"b_type"`
	BSummary   string     `json:"b_summary"`
}

type nodeRecord struct {
	ID      *models.RecordID `json:"id"`
	UUID    string           `json:"uuid"`
	Name    string           `json:"name"`
	Type    string           `json:"type"`
	Summary string           `json:"summary"`
}

type relRow struct {
	RUUID      string     `json:"r_uuid"`
	RType      string     `json:"r_type"`
	RFact      string     `json:"r_fact"`
	RCreatedAt time.Time  `json:"r_created_at"`
	RValidAt   time.Time  `json:"r_valid_at"`
	RInvalidAt *time.Time `json:"r_invalid_at"`
	NUUID      string     `json:"n_uuid"`
	NName      string     `json:"n_name"`
	NType      string     `json:"n_type"`
	NSummary   string     `json:"n_summary"`
	IsOutgoing bool       `json:"is_outgoing"`
}

type relationRecord struct {
	UUID       string     `json:"uuid"`
	Type       string     `json:"type"`
	Fact       string     `json:"fact"`
	CreatedAt  time.Time  `json:"created_at"`
	ValidAt    time.Time  `json:"valid_at"`
	InvalidAt  *time.Time `json:"invalid_at"`
	InUUID     string     `json:"in_uuid"`
	OutUUID    string     `json:"out_uuid"`
}
