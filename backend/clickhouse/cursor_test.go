package clickhouse

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/smithy-go/ptr"
	modelInputs "github.com/highlight-run/highlight/backend/private-graph/graph/model"
	"github.com/stretchr/testify/assert"
)

func TestGetConnectionAfter(t *testing.T) {
	zeroEdges := []*Edge[modelInputs.Log]{}
	oneEdge := []*Edge[modelInputs.Log]{
		{
			Cursor: "cursor",
		},
	}

	manyEdges := []*Edge[modelInputs.Log]{}
	for i := 1; i <= LogsLimit+1; i++ {
		manyEdges = append(manyEdges, &Edge[modelInputs.Log]{
			Cursor: "cursor",
		})
	}

	type lc = Connection[modelInputs.Log]

	conn := getConnection(zeroEdges, Pagination{
		After: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: zeroEdges,
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     false,
			HasPreviousPage: true,
			StartCursor:     "",
			EndCursor:       "",
		},
	}, conn)

	conn = getConnection(oneEdge, Pagination{
		After: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: oneEdge,
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     false,
			HasPreviousPage: true,
			StartCursor:     "cursor",
			EndCursor:       "cursor",
		},
	}, conn)

	conn = getConnection(manyEdges, Pagination{
		After: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: manyEdges[:LogsLimit],
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     true,
			HasPreviousPage: true,
			StartCursor:     "cursor",
			EndCursor:       "cursor",
		},
	}, conn)
}

func TestGetConnectionBefore(t *testing.T) {
	zeroEdges := []*Edge[modelInputs.Log]{}
	oneEdge := []*Edge[modelInputs.Log]{
		{
			Cursor: "cursor",
		},
	}

	manyEdges := []*Edge[modelInputs.Log]{}
	for i := 1; i <= LogsLimit+1; i++ {
		manyEdges = append(manyEdges, &Edge[modelInputs.Log]{
			Cursor: "cursor",
		})
	}

	type lc = Connection[modelInputs.Log]

	conn := getConnection(zeroEdges, Pagination{
		Before: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: zeroEdges,
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     true,
			HasPreviousPage: false,
			StartCursor:     "",
			EndCursor:       "",
		},
	}, conn)

	conn = getConnection(oneEdge, Pagination{
		Before: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: oneEdge,
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     true,
			HasPreviousPage: false,
			StartCursor:     "cursor",
			EndCursor:       "cursor",
		},
	}, conn)

	conn = getConnection(manyEdges, Pagination{
		Before: ptr.String("cursor"),
	})

	assert.Equal(t, &lc{
		Edges: manyEdges[1:LogsLimit],
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     true,
			HasPreviousPage: true,
			StartCursor:     "cursor",
			EndCursor:       "cursor",
		},
	}, conn)
}

func TestGetConnectionNoPagination(t *testing.T) {
	zeroEdges := []*Edge[modelInputs.Log]{}
	manyEdges := []*Edge[modelInputs.Log]{}
	for i := 1; i <= LogsLimit+1; i++ {
		manyEdges = append(manyEdges, &Edge[modelInputs.Log]{
			Cursor: "cursor",
		})
	}

	type lc = Connection[modelInputs.Log]

	connection := getConnection(zeroEdges, Pagination{})

	assert.Equal(t, &lc{
		Edges: zeroEdges,
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     false,
			HasPreviousPage: false,
			StartCursor:     "",
			EndCursor:       "",
		},
	}, connection)

	connection = getConnection(manyEdges, Pagination{})

	assert.Equal(t, &lc{
		Edges: manyEdges[:LogsLimit],
		PageInfo: &modelInputs.PageInfo{
			HasNextPage:     true,
			HasPreviousPage: false,
			StartCursor:     "cursor",
			EndCursor:       "cursor",
		},
	}, connection)
}

func TestEncodeDecode(t *testing.T) {
	now := time.Now()
	cursor := encodeCursor(now, "uuid")

	timestamp, uuid, err := decodeCursor(cursor)
	assert.NoError(t, err)

	// decoded timestamp should have second precision
	assert.Equal(t, timestamp.UnixNano(), now.Unix()*10e8)
	assert.Equal(t, "uuid", uuid)
}

func TestClickhouseDecode(t *testing.T) {
	ctx := context.Background()
	client, err := setupClickhouseTestDB()

	assert.NoError(t, err)

	defer func() {
		err := client.conn.Exec(ctx, fmt.Sprintf("TRUNCATE TABLE %s", LogsTable))
		assert.NoError(t, err)
	}()

	now := time.Now()
	rows := []*LogRow{
		NewLogRow(now, 1),
	}
	assert.NoError(t, client.BatchWriteLogRows(ctx, rows))

	payload, err := client.ReadLogs(ctx, 1, modelInputs.QueryInput{
		DateRange: makeDateWithinRange(now),
	}, Pagination{})
	assert.NoError(t, err)

	cursor := payload.Edges[0].Cursor
	assert.NoError(t, err)

	timestamp, _, err := decodeCursor(cursor)
	assert.NoError(t, err)
	assert.Equal(t, timestamp.UnixNano(), payload.Edges[0].Node.Timestamp.UnixNano())
}
