package mask

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BugenZhao/sql-masker/tidb"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/format"
	"github.com/pingcap/parser/mysql"
	ptypes "github.com/pingcap/parser/types"
	"github.com/pingcap/tidb/kv"
)

type Stats struct {
	All         uint64
	Problematic uint64
	Success     uint64
}

func (s Stats) Summary() {
	fmt.Printf(`

====Summary====
Success      %d
Problematic  %d
Failed       %d
Total        %d
	`,
		s.Success, s.Problematic, s.All-s.Success-s.Problematic, s.All)
}

type worker struct {
	Stats    Stats
	db       *tidb.Context
	maskFunc MaskFunc
}

func newWorker(db *tidb.Context, maskFunc MaskFunc) *worker {
	return &worker{
		db:       db,
		maskFunc: maskFunc,
	}
}

func (w *worker) replaceValue(sql string) (ast.StmtNode, ExprMap, error) {
	node, err := w.db.ParseOne(sql)
	if err != nil {
		return nil, nil, err
	}
	v := NewReplaceVisitor(ReplaceModeValue)
	newNode, _ := node.Accept(v)

	return newNode.(ast.StmtNode), v.OriginExprs, nil
}

func (w *worker) replaceParamMarker(sql string) (ast.StmtNode, []ReplaceMarker, error) {
	node, err := w.db.ParseOne(sql)
	if err != nil {
		return nil, nil, err
	}
	v := NewReplaceVisitor(ReplaceModeParamMarker)
	newNode, _ := node.Accept(v)

	markers := make([]ReplaceMarker, 0, len(v.Offsets))
	for k := range v.Offsets {
		markers = append(markers, k)
	}
	sort.Slice(markers, func(i, j int) bool { return v.Offsets[markers[i]] < v.Offsets[markers[j]] })

	return newNode.(ast.StmtNode), markers, nil
}

func (w *worker) restore(stmtNode ast.StmtNode, originExprs ExprMap, inferredTypes TypeMap) (string, error) {
	v := NewRestoreVisitor(originExprs, inferredTypes, w.maskFunc)
	newNode, ok := stmtNode.Accept(v)
	if !ok {
		return "", v.err
	}

	buf := &strings.Builder{}
	restoreFlags := format.DefaultRestoreFlags | format.RestoreStringWithoutDefaultCharset
	restoreCtx := format.NewRestoreCtx(restoreFlags, buf)
	err := newNode.Restore(restoreCtx)
	if err != nil {
		return "", err
	}

	newSQL := buf.String()
	return newSQL, v.err
}

func (w *worker) infer(stmtNode ast.StmtNode) (TypeMap, error) {
	execStmt, err := w.db.CompileStmtNode(stmtNode)
	if err != nil {
		return nil, err
	}

	b := NewCastGraphBuilder()
	err = b.Build(execStmt.Plan)
	if err != nil {
		return nil, err
	}

	inferredTypes := make(TypeMap)
	for _, c := range b.Constants {
		tp := b.Graph.InferType(c)

		s, err := c.Value.ToString()
		if err != nil {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		inferredTypes[ReplaceMarker(f)] = tp
	}
	for _, h := range b.Handles {
		switch h := h.(type) {
		case kv.IntHandle:
			inferredTypes[ReplaceMarker(h.IntValue())] = ptypes.NewFieldType(mysql.TypeLong) // todo: which tp ?
		default:
			// ignore common handle for clustered index
		}
	}

	return inferredTypes, nil
}

func (w *worker) maskOneQuery(sql string) (string, error) {
	replacedStmtNode, originExprs, err := w.replaceValue(sql)
	if err != nil {
		return sql, err
	}

	inferredTypes, err := w.infer(replacedStmtNode)
	if err != nil {
		return sql, err
	}

	newSQL, err := w.restore(replacedStmtNode, originExprs, inferredTypes)
	if err != nil {
		if newSQL == "" {
			return sql, err
		} else {
			return newSQL, err
		}
	}

	return newSQL, nil
}
