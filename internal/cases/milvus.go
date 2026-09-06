package cases

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const collection = "aiops_cases"

// MilvusStore Milvus 向量检索实现。
type MilvusStore struct {
	cli   client.Client
	falls *PGStore // Milvus 操作失败时的兜底
}

// NewMilvus 连接 Milvus 并确保集合存在；任意失败返回错误（调用方走 PG 兜底）。
func NewMilvus(ctx context.Context, addr string, falls *PGStore) (*MilvusStore, error) {
	cli, err := client.NewClient(ctx, client.Config{Address: addr})
	if err != nil {
		return nil, fmt.Errorf("milvus 连接失败: %w", err)
	}
	exists, err := cli.HasCollection(ctx, collection)
	if err != nil {
		return nil, fmt.Errorf("milvus HasCollection: %w", err)
	}
	if !exists {
		sch := entity.NewSchema().WithName(collection).WithDescription("AIOps resolved-ticket cases").
			WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true).WithIsAutoID(true)).
			WithField(entity.NewField().WithName("ticket_id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
			WithField(entity.NewField().WithName("type").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32)).
			WithField(entity.NewField().WithName("service").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
			WithField(entity.NewField().WithName("text").WithDataType(entity.FieldTypeVarChar).WithMaxLength(4096)).
			WithField(entity.NewField().WithName("root_cause").WithDataType(entity.FieldTypeVarChar).WithMaxLength(2048)).
			WithField(entity.NewField().WithName("created_at").WithDataType(entity.FieldTypeInt64)).
			WithField(entity.NewField().WithName("vector").WithDataType(entity.FieldTypeFloatVector).WithDim(64))
		if err := cli.CreateCollection(ctx, sch, 1); err != nil {
			return nil, fmt.Errorf("milvus 建集合: %w", err)
		}
		idx := entity.NewGenericIndex("vector", entity.AUTOINDEX, map[string]string{})
		if err := cli.CreateIndex(ctx, collection, "vector", idx, false); err != nil {
			return nil, fmt.Errorf("milvus 建索引: %w", err)
		}
	}
	// 检索前必须 Load；已存在的集合同样确保加载。
	if err := cli.LoadCollection(ctx, collection, false); err != nil {
		return nil, fmt.Errorf("milvus 加载集合: %w", err)
	}
	slog.Info("Milvus 案例库就绪", "addr", addr, "collection", collection)
	return &MilvusStore{cli: cli, falls: falls}, nil
}

// Backend 实现标识。
func (s *MilvusStore) Backend() string { return "milvus" }

// Save 向量化写入；失败兜底 PG。
func (s *MilvusStore) Save(ctx context.Context, c *Case) error {
	text := TextOf(c)
	if len(text) > 4000 {
		text = text[:4000]
	}
	cols := []entity.Column{
		entity.NewColumnVarChar("ticket_id", []string{c.TicketID}),
		entity.NewColumnVarChar("type", []string{c.Type}),
		entity.NewColumnVarChar("service", []string{c.Service}),
		entity.NewColumnVarChar("text", []string{text}),
		entity.NewColumnVarChar("root_cause", []string{c.RootCause}),
		entity.NewColumnInt64("created_at", []int64{c.CreatedAt.Unix()}),
		entity.NewColumnFloatVector("vector", 64, [][]float32{Embed(text)}),
	}
	if _, err := s.cli.Insert(ctx, collection, "", cols...); err != nil {
		slog.Warn("Milvus 写入失败，兜底 PG", "err", err)
		return s.falls.Save(ctx, c)
	}
	return s.cli.Flush(ctx, collection, false)
}

// List 最近案例（Milvus 不支持便捷全量列举，转发 PG）。
func (s *MilvusStore) List(ctx context.Context, limit int) ([]Case, error) {
	return s.falls.List(ctx, limit)
}

// Search 向量检索 topK；失败兜底 PG。
func (s *MilvusStore) Search(ctx context.Context, query string, topK int) ([]Scored, error) {
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sp, err := entity.NewIndexFlatSearchParam()
	if err != nil {
		return nil, err
	}
	rs, err := s.cli.Search(sctx, collection, nil, "",
		[]string{"ticket_id", "type", "service", "text", "root_cause", "created_at"},
		[]entity.Vector{entity.FloatVector(Embed(query))},
		"vector", entity.COSINE, topK, sp)
	if err != nil {
		slog.Warn("Milvus 检索失败，兜底 PG", "err", err)
		return s.falls.Search(ctx, query, topK)
	}

	// SDK 结果为列式：Fields[j].ValueByIdx(i) 取第 i 行。
	out := []Scored{}
	if len(rs) == 0 {
		return out, nil
	}
	sr := rs[0]
	n := sr.IDs.Len()
	getStr := func(i int, name string) string {
		for _, col := range sr.Fields {
			if col.Name() != name {
				continue
			}
			if vc, ok := col.(*entity.ColumnVarChar); ok {
				if v, err := vc.ValueByIdx(i); err == nil {
					return v
				}
			}
		}
		return ""
	}
	for i := 0; i < n && i < len(sr.Scores); i++ {
		out = append(out, Scored{
			Case: Case{
				TicketID:  getStr(i, "ticket_id"),
				Type:      getStr(i, "type"),
				Service:   getStr(i, "service"),
				Summary:   getStr(i, "text"),
				RootCause: getStr(i, "root_cause"),
			},
			Score: float64(sr.Scores[i]),
		})
	}
	// 修正 created_at（上面误用 score 占位）：从 Fields 读取真实时间戳。
	for _, col := range sr.Fields {
		if col.Name() == "created_at" {
			if ic, ok := col.(*entity.ColumnInt64); ok {
				for i := range out {
					if v, err := ic.ValueByIdx(i); err == nil {
						out[i].CreatedAt = time.Unix(v, 0).UTC()
					}
				}
			}
		}
	}
	return out, nil
}
