// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/collate"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/plancodec"
	"github.com/pingcap/tidb/util/ranger"
	"github.com/pingcap/tidb/util/set"
	"go.uber.org/zap"
)

func (p *LogicalUnionScan) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `UnionScan` is not supported now.")
		return nil, true, nil
	}
	childProp := prop.CloneEssentialFields()
	us := PhysicalUnionScan{
		Conditions: p.conditions,
		HandleCols: p.handleCols,
	}.Init(p.ctx, p.stats, p.blockOffset, childProp)
	return []PhysicalPlan{us}, true, nil
}

func getMaxSortPrefix(sortCols, allCols []*expression.Column) []int {
	tmpSchema := expression.NewSchema(allCols...)
	sortColOffsets := make([]int, 0, len(sortCols))
	for _, sortCol := range sortCols {
		offset := tmpSchema.ColumnIndex(sortCol)
		if offset == -1 {
			return sortColOffsets
		}
		sortColOffsets = append(sortColOffsets, offset)
	}
	return sortColOffsets
}

func findMaxPrefixLen(candidates [][]*expression.Column, keys []*expression.Column) int {
	maxLen := 0
	for _, candidateKeys := range candidates {
		matchedLen := 0
		for i := range keys {
			if i < len(candidateKeys) && keys[i].Equal(nil, candidateKeys[i]) {
				matchedLen++
			} else {
				break
			}
		}
		if matchedLen > maxLen {
			maxLen = matchedLen
		}
	}
	return maxLen
}

func (p *LogicalJoin) moveEqualToOtherConditions(offsets []int) []expression.Expression {
	// Construct used equal condition set based on the equal condition offsets.
	usedEqConds := set.NewIntSet()
	for _, eqCondIdx := range offsets {
		usedEqConds.Insert(eqCondIdx)
	}

	// Construct otherConds, which is composed of the original other conditions
	// and the remained unused equal conditions.
	numOtherConds := len(p.OtherConditions) + len(p.EqualConditions) - len(usedEqConds)
	otherConds := make([]expression.Expression, len(p.OtherConditions), numOtherConds)
	copy(otherConds, p.OtherConditions)
	for eqCondIdx := range p.EqualConditions {
		if !usedEqConds.Exist(eqCondIdx) {
			otherConds = append(otherConds, p.EqualConditions[eqCondIdx])
		}
	}

	return otherConds
}

// Only if the input required prop is the prefix fo join keys, we can pass through this property.
func (p *PhysicalMergeJoin) tryToGetChildReqProp(prop *property.PhysicalProperty) ([]*property.PhysicalProperty, bool) {
	all, desc := prop.AllSameOrder()
	lProp := property.NewPhysicalProperty(property.RootTaskType, p.LeftJoinKeys, desc, math.MaxFloat64, false)
	rProp := property.NewPhysicalProperty(property.RootTaskType, p.RightJoinKeys, desc, math.MaxFloat64, false)
	if !prop.IsEmpty() {
		// sort merge join fits the cases of massive ordered data, so desc scan is always expensive.
		if !all {
			return nil, false
		}
		if !prop.IsPrefix(lProp) && !prop.IsPrefix(rProp) {
			return nil, false
		}
		if prop.IsPrefix(rProp) && p.JoinType == LeftOuterJoin {
			return nil, false
		}
		if prop.IsPrefix(lProp) && p.JoinType == RightOuterJoin {
			return nil, false
		}
	}

	return []*property.PhysicalProperty{lProp, rProp}, true
}

func (p *LogicalJoin) checkJoinKeyCollation(leftKeys, rightKeys []*expression.Column) bool {
	// if a left key and its corresponding right key have different collation, don't use MergeJoin since
	// the their children may sort their records in different ways
	for i := range leftKeys {
		lt := leftKeys[i].RetType
		rt := rightKeys[i].RetType
		if (lt.EvalType() == types.ETString && rt.EvalType() == types.ETString) &&
			(leftKeys[i].RetType.Charset != rightKeys[i].RetType.Charset ||
				leftKeys[i].RetType.Collate != rightKeys[i].RetType.Collate) {
			return false
		}
	}
	return true
}

// GetMergeJoin convert the logical join to physical merge join based on the physical property.
func (p *LogicalJoin) GetMergeJoin(prop *property.PhysicalProperty, schema *expression.Schema, statsInfo *property.StatsInfo, leftStatsInfo *property.StatsInfo, rightStatsInfo *property.StatsInfo) []PhysicalPlan {
	joins := make([]PhysicalPlan, 0, len(p.leftProperties)+1)
	// The leftProperties caches all the possible properties that are provided by its children.
	leftJoinKeys, rightJoinKeys, isNullEQ, hasNullEQ := p.GetJoinKeys()

	// EnumType/SetType Unsupported: merge join conflicts with index order.
	// ref: https://github.com/pingcap/tidb/issues/24473, https://github.com/pingcap/tidb/issues/25669
	for _, leftKey := range leftJoinKeys {
		if leftKey.RetType.Tp == mysql.TypeEnum || leftKey.RetType.Tp == mysql.TypeSet {
			return nil
		}
	}
	for _, rightKey := range rightJoinKeys {
		if rightKey.RetType.Tp == mysql.TypeEnum || rightKey.RetType.Tp == mysql.TypeSet {
			return nil
		}
	}

	// TODO: support null equal join keys for merge join
	if hasNullEQ {
		return nil
	}
	for _, lhsChildProperty := range p.leftProperties {
		offsets := getMaxSortPrefix(lhsChildProperty, leftJoinKeys)
		// If not all equal conditions hit properties. We ban merge join heuristically. Because in this case, merge join
		// may get a very low performance. In executor, executes join results before other conditions filter it.
		if len(offsets) < len(leftJoinKeys) {
			continue
		}

		leftKeys := lhsChildProperty[:len(offsets)]
		rightKeys := expression.NewSchema(rightJoinKeys...).ColumnsByIndices(offsets)
		newIsNullEQ := make([]bool, 0, len(offsets))
		for _, offset := range offsets {
			newIsNullEQ = append(newIsNullEQ, isNullEQ[offset])
		}

		prefixLen := findMaxPrefixLen(p.rightProperties, rightKeys)
		if prefixLen == 0 {
			continue
		}

		leftKeys = leftKeys[:prefixLen]
		rightKeys = rightKeys[:prefixLen]
		newIsNullEQ = newIsNullEQ[:prefixLen]
		if !p.checkJoinKeyCollation(leftKeys, rightKeys) {
			continue
		}
		offsets = offsets[:prefixLen]
		baseJoin := basePhysicalJoin{
			JoinType:        p.JoinType,
			LeftConditions:  p.LeftConditions,
			RightConditions: p.RightConditions,
			DefaultValues:   p.DefaultValues,
			LeftJoinKeys:    leftKeys,
			RightJoinKeys:   rightKeys,
			IsNullEQ:        newIsNullEQ,
		}
		mergeJoin := PhysicalMergeJoin{basePhysicalJoin: baseJoin}.Init(p.ctx, statsInfo.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset)
		mergeJoin.SetSchema(schema)
		mergeJoin.OtherConditions = p.moveEqualToOtherConditions(offsets)
		mergeJoin.initCompareFuncs()
		if reqProps, ok := mergeJoin.tryToGetChildReqProp(prop); ok {
			// Adjust expected count for children nodes.
			if prop.ExpectedCnt < statsInfo.RowCount {
				expCntScale := prop.ExpectedCnt / statsInfo.RowCount
				reqProps[0].ExpectedCnt = leftStatsInfo.RowCount * expCntScale
				reqProps[1].ExpectedCnt = rightStatsInfo.RowCount * expCntScale
			}
			mergeJoin.childrenReqProps = reqProps
			_, desc := prop.AllSameOrder()
			mergeJoin.Desc = desc
			joins = append(joins, mergeJoin)
		}
	}
	// If TiDB_SMJ hint is existed, it should consider enforce merge join,
	// because we can't trust lhsChildProperty completely.
	if (p.preferJoinType & preferMergeJoin) > 0 {
		joins = append(joins, p.getEnforcedMergeJoin(prop, schema, statsInfo)...)
	}

	return joins
}

// Change JoinKeys order, by offsets array
// offsets array is generate by prop check
func getNewJoinKeysByOffsets(oldJoinKeys []*expression.Column, offsets []int) []*expression.Column {
	newKeys := make([]*expression.Column, 0, len(oldJoinKeys))
	for _, offset := range offsets {
		newKeys = append(newKeys, oldJoinKeys[offset])
	}
	for pos, key := range oldJoinKeys {
		isExist := false
		for _, p := range offsets {
			if p == pos {
				isExist = true
				break
			}
		}
		if !isExist {
			newKeys = append(newKeys, key)
		}
	}
	return newKeys
}

func getNewNullEQByOffsets(oldNullEQ []bool, offsets []int) []bool {
	newNullEQ := make([]bool, 0, len(oldNullEQ))
	for _, offset := range offsets {
		newNullEQ = append(newNullEQ, oldNullEQ[offset])
	}
	for pos, key := range oldNullEQ {
		isExist := false
		for _, p := range offsets {
			if p == pos {
				isExist = true
				break
			}
		}
		if !isExist {
			newNullEQ = append(newNullEQ, key)
		}
	}
	return newNullEQ
}

func (p *LogicalJoin) getEnforcedMergeJoin(prop *property.PhysicalProperty, schema *expression.Schema, statsInfo *property.StatsInfo) []PhysicalPlan {
	// Check whether SMJ can satisfy the required property
	leftJoinKeys, rightJoinKeys, isNullEQ, hasNullEQ := p.GetJoinKeys()
	// TODO: support null equal join keys for merge join
	if hasNullEQ {
		return nil
	}
	offsets := make([]int, 0, len(leftJoinKeys))
	all, desc := prop.AllSameOrder()
	if !all {
		return nil
	}
	for _, item := range prop.SortItems {
		isExist := false
		for joinKeyPos := 0; joinKeyPos < len(leftJoinKeys); joinKeyPos++ {
			var key *expression.Column
			if item.Col.Equal(p.ctx, leftJoinKeys[joinKeyPos]) {
				key = leftJoinKeys[joinKeyPos]
			}
			if item.Col.Equal(p.ctx, rightJoinKeys[joinKeyPos]) {
				key = rightJoinKeys[joinKeyPos]
			}
			if key == nil {
				continue
			}
			for i := 0; i < len(offsets); i++ {
				if offsets[i] == joinKeyPos {
					isExist = true
					break
				}
			}
			if !isExist {
				offsets = append(offsets, joinKeyPos)
			}
			isExist = true
			break
		}
		if !isExist {
			return nil
		}
	}
	// Generate the enforced sort merge join
	leftKeys := getNewJoinKeysByOffsets(leftJoinKeys, offsets)
	rightKeys := getNewJoinKeysByOffsets(rightJoinKeys, offsets)
	newNullEQ := getNewNullEQByOffsets(isNullEQ, offsets)
	otherConditions := make([]expression.Expression, len(p.OtherConditions), len(p.OtherConditions)+len(p.EqualConditions))
	copy(otherConditions, p.OtherConditions)
	if !p.checkJoinKeyCollation(leftKeys, rightKeys) {
		// if the join keys' collation are conflicted, we use the empty join key
		// and move EqualConditions to OtherConditions.
		leftKeys = nil
		rightKeys = nil
		newNullEQ = nil
		otherConditions = append(otherConditions, expression.ScalarFuncs2Exprs(p.EqualConditions)...)
	}
	lProp := property.NewPhysicalProperty(property.RootTaskType, leftKeys, desc, math.MaxFloat64, true)
	rProp := property.NewPhysicalProperty(property.RootTaskType, rightKeys, desc, math.MaxFloat64, true)
	baseJoin := basePhysicalJoin{
		JoinType:        p.JoinType,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		DefaultValues:   p.DefaultValues,
		LeftJoinKeys:    leftKeys,
		RightJoinKeys:   rightKeys,
		IsNullEQ:        newNullEQ,
		OtherConditions: otherConditions,
	}
	enforcedPhysicalMergeJoin := PhysicalMergeJoin{basePhysicalJoin: baseJoin, Desc: desc}.Init(p.ctx, statsInfo.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset)
	enforcedPhysicalMergeJoin.SetSchema(schema)
	enforcedPhysicalMergeJoin.childrenReqProps = []*property.PhysicalProperty{lProp, rProp}
	enforcedPhysicalMergeJoin.initCompareFuncs()
	return []PhysicalPlan{enforcedPhysicalMergeJoin}
}

func (p *PhysicalMergeJoin) initCompareFuncs() {
	p.CompareFuncs = make([]expression.CompareFunc, 0, len(p.LeftJoinKeys))
	for i := range p.LeftJoinKeys {
		p.CompareFuncs = append(p.CompareFuncs, expression.GetCmpFunction(p.ctx, p.LeftJoinKeys[i], p.RightJoinKeys[i]))
	}
}

// ForceUseOuterBuild4Test is a test option to control forcing use outer input as build.
// TODO: use hint and remove this variable
var ForceUseOuterBuild4Test = false

// ForcedHashLeftJoin4Test is a test option to force using HashLeftJoin
// TODO: use hint and remove this variable
var ForcedHashLeftJoin4Test = false

func (p *LogicalJoin) getHashJoins(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() { // hash join doesn't promise any orders
		return nil
	}
	joins := make([]PhysicalPlan, 0, 2)
	switch p.JoinType {
	case SemiJoin, AntiSemiJoin, LeftOuterSemiJoin, AntiLeftOuterSemiJoin:
		joins = append(joins, p.getHashJoin(prop, 1, false))
	case LeftOuterJoin:
		if ForceUseOuterBuild4Test {
			joins = append(joins, p.getHashJoin(prop, 1, true))
		} else {
			joins = append(joins, p.getHashJoin(prop, 1, false))
			joins = append(joins, p.getHashJoin(prop, 1, true))
		}
	case RightOuterJoin:
		if ForceUseOuterBuild4Test {
			joins = append(joins, p.getHashJoin(prop, 0, true))
		} else {
			joins = append(joins, p.getHashJoin(prop, 0, false))
			joins = append(joins, p.getHashJoin(prop, 0, true))
		}
	case InnerJoin:
		if ForcedHashLeftJoin4Test {
			joins = append(joins, p.getHashJoin(prop, 1, false))
		} else {
			joins = append(joins, p.getHashJoin(prop, 1, false))
			joins = append(joins, p.getHashJoin(prop, 0, false))
		}
	}
	return joins
}

func (p *LogicalJoin) getHashJoin(prop *property.PhysicalProperty, innerIdx int, useOuterToBuild bool) *PhysicalHashJoin {
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[innerIdx] = &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64}
	chReqProps[1-innerIdx] = &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64}
	if prop.ExpectedCnt < p.stats.RowCount {
		expCntScale := prop.ExpectedCnt / p.stats.RowCount
		chReqProps[1-innerIdx].ExpectedCnt = p.children[1-innerIdx].statsInfo().RowCount * expCntScale
	}
	hashJoin := NewPhysicalHashJoin(p, innerIdx, useOuterToBuild, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), chReqProps...)
	hashJoin.SetSchema(p.schema)
	return hashJoin
}

// When inner plan is TableReader, the parameter `ranges` will be nil. Because pk only have one column. So all of its range
// is generated during execution time.
func (p *LogicalJoin) constructIndexJoin(
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask task,
	ranges []*ranger.Range,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *ColWithCmpFuncManager,
) []PhysicalPlan {
	joinType := p.JoinType
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
		isNullEQ      []bool
		hasNullEQ     bool
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, isNullEQ, hasNullEQ = p.GetJoinKeys()
	}
	// TODO: support null equal join keys for index join
	if hasNullEQ {
		return nil
	}
	chReqProps := make([]*property.PhysicalProperty, 2)
	chReqProps[outerIdx] = &property.PhysicalProperty{TaskTp: property.RootTaskType, ExpectedCnt: math.MaxFloat64, SortItems: prop.SortItems}
	if prop.ExpectedCnt < p.stats.RowCount {
		expCntScale := prop.ExpectedCnt / p.stats.RowCount
		chReqProps[outerIdx].ExpectedCnt = p.children[outerIdx].statsInfo().RowCount * expCntScale
	}
	newInnerKeys := make([]*expression.Column, 0, len(innerJoinKeys))
	newOuterKeys := make([]*expression.Column, 0, len(outerJoinKeys))
	newIsNullEQ := make([]bool, 0, len(isNullEQ))
	newKeyOff := make([]int, 0, len(keyOff2IdxOff))
	newOtherConds := make([]expression.Expression, len(p.OtherConditions), len(p.OtherConditions)+len(p.EqualConditions))
	copy(newOtherConds, p.OtherConditions)
	for keyOff, idxOff := range keyOff2IdxOff {
		if keyOff2IdxOff[keyOff] < 0 {
			newOtherConds = append(newOtherConds, p.EqualConditions[keyOff])
			continue
		}
		newInnerKeys = append(newInnerKeys, innerJoinKeys[keyOff])
		newOuterKeys = append(newOuterKeys, outerJoinKeys[keyOff])
		newIsNullEQ = append(newIsNullEQ, isNullEQ[keyOff])
		newKeyOff = append(newKeyOff, idxOff)
	}

	var outerHashKeys, innerHashKeys []*expression.Column
	outerHashKeys, innerHashKeys = make([]*expression.Column, len(newOuterKeys)), make([]*expression.Column, len(newInnerKeys))
	copy(outerHashKeys, newOuterKeys)
	copy(innerHashKeys, newInnerKeys)
	// we can use the `col <eq> col` in `OtherCondition` to build the hashtable to avoid the unnecessary calculating.
	for i := len(newOtherConds) - 1; i >= 0; i = i - 1 {
		switch c := newOtherConds[i].(type) {
		case *expression.ScalarFunction:
			if c.FuncName.L == ast.EQ {
				lhs, ok1 := c.GetArgs()[0].(*expression.Column)
				rhs, ok2 := c.GetArgs()[1].(*expression.Column)
				if ok1 && ok2 {
					if lhs.InOperand || rhs.InOperand {
						// if this other-cond is from a `[not] in` sub-query, do not convert it into eq-cond since
						// IndexJoin cannot deal with NULL correctly in this case; please see #25799 for more details.
						continue
					}
					outerSchema, innerSchema := p.Children()[outerIdx].Schema(), p.Children()[1-outerIdx].Schema()
					if outerSchema.Contains(lhs) && innerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, lhs)
						innerHashKeys = append(innerHashKeys, rhs)
					} else if innerSchema.Contains(lhs) && outerSchema.Contains(rhs) {
						outerHashKeys = append(outerHashKeys, rhs)
						innerHashKeys = append(innerHashKeys, lhs)
					}
					newOtherConds = append(newOtherConds[:i], newOtherConds[i+1:]...)
				}
			}
		default:
			continue
		}
	}

	baseJoin := basePhysicalJoin{
		InnerChildIdx:   1 - outerIdx,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: newOtherConds,
		JoinType:        joinType,
		OuterJoinKeys:   newOuterKeys,
		InnerJoinKeys:   newInnerKeys,
		IsNullEQ:        newIsNullEQ,
		DefaultValues:   p.DefaultValues,
	}

	join := PhysicalIndexJoin{
		basePhysicalJoin: baseJoin,
		innerTask:        innerTask,
		KeyOff2IdxOff:    newKeyOff,
		Ranges:           ranges,
		CompareFilters:   compareFilters,
		OuterHashKeys:    outerHashKeys,
		InnerHashKeys:    innerHashKeys,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, chReqProps...)
	if path != nil {
		join.IdxColLens = path.IdxColLens
	}
	join.SetSchema(p.schema)
	return []PhysicalPlan{join}
}

func (p *LogicalJoin) constructIndexMergeJoin(
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask task,
	ranges []*ranger.Range,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *ColWithCmpFuncManager,
) []PhysicalPlan {
	indexJoins := p.constructIndexJoin(prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, compareFilters)
	indexMergeJoins := make([]PhysicalPlan, 0, len(indexJoins))
	for _, plan := range indexJoins {
		join := plan.(*PhysicalIndexJoin)
		// Index merge join can't handle hash keys. So we ban it heuristically.
		if len(join.InnerHashKeys) > len(join.InnerJoinKeys) {
			return nil
		}

		// EnumType/SetType Unsupported: merge join conflicts with index order.
		// ref: https://github.com/pingcap/tidb/issues/24473, https://github.com/pingcap/tidb/issues/25669
		for _, innerKey := range join.InnerJoinKeys {
			if innerKey.RetType.Tp == mysql.TypeEnum || innerKey.RetType.Tp == mysql.TypeSet {
				return nil
			}
		}
		for _, outerKey := range join.OuterJoinKeys {
			if outerKey.RetType.Tp == mysql.TypeEnum || outerKey.RetType.Tp == mysql.TypeSet {
				return nil
			}
		}

		hasPrefixCol := false
		for _, l := range join.IdxColLens {
			if l != types.UnspecifiedLength {
				hasPrefixCol = true
				break
			}
		}
		// If index column has prefix length, the merge join can not guarantee the relevance
		// between index and join keys. So we should skip this case.
		// For more details, please check the following code and comments.
		if hasPrefixCol {
			continue
		}

		// keyOff2KeyOffOrderByIdx is map the join keys offsets to [0, len(joinKeys)) ordered by the
		// join key position in inner index.
		keyOff2KeyOffOrderByIdx := make([]int, len(join.OuterJoinKeys))
		keyOffMapList := make([]int, len(join.KeyOff2IdxOff))
		copy(keyOffMapList, join.KeyOff2IdxOff)
		keyOffMap := make(map[int]int, len(keyOffMapList))
		for i, idxOff := range keyOffMapList {
			keyOffMap[idxOff] = i
		}
		sort.Slice(keyOffMapList, func(i, j int) bool { return keyOffMapList[i] < keyOffMapList[j] })
		keyIsIndexPrefix := true
		for keyOff, idxOff := range keyOffMapList {
			if keyOff != idxOff {
				keyIsIndexPrefix = false
				break
			}
			keyOff2KeyOffOrderByIdx[keyOffMap[idxOff]] = keyOff
		}
		if !keyIsIndexPrefix {
			continue
		}
		// isOuterKeysPrefix means whether the outer join keys are the prefix of the prop items.
		isOuterKeysPrefix := len(join.OuterJoinKeys) <= len(prop.SortItems)
		compareFuncs := make([]expression.CompareFunc, 0, len(join.OuterJoinKeys))
		outerCompareFuncs := make([]expression.CompareFunc, 0, len(join.OuterJoinKeys))

		for i := range join.KeyOff2IdxOff {
			if isOuterKeysPrefix && !prop.SortItems[i].Col.Equal(nil, join.OuterJoinKeys[keyOff2KeyOffOrderByIdx[i]]) {
				isOuterKeysPrefix = false
			}
			compareFuncs = append(compareFuncs, expression.GetCmpFunction(p.ctx, join.OuterJoinKeys[i], join.InnerJoinKeys[i]))
			outerCompareFuncs = append(outerCompareFuncs, expression.GetCmpFunction(p.ctx, join.OuterJoinKeys[i], join.OuterJoinKeys[i]))
		}
		// canKeepOuterOrder means whether the prop items are the prefix of the outer join keys.
		canKeepOuterOrder := len(prop.SortItems) <= len(join.OuterJoinKeys)
		for i := 0; canKeepOuterOrder && i < len(prop.SortItems); i++ {
			if !prop.SortItems[i].Col.Equal(nil, join.OuterJoinKeys[keyOff2KeyOffOrderByIdx[i]]) {
				canKeepOuterOrder = false
			}
		}
		// Since index merge join requires prop items the prefix of outer join keys
		// or outer join keys the prefix of the prop items. So we need `canKeepOuterOrder` or
		// `isOuterKeysPrefix` to be true.
		if canKeepOuterOrder || isOuterKeysPrefix {
			indexMergeJoin := PhysicalIndexMergeJoin{
				PhysicalIndexJoin:       *join,
				KeyOff2KeyOffOrderByIdx: keyOff2KeyOffOrderByIdx,
				NeedOuterSort:           !isOuterKeysPrefix,
				CompareFuncs:            compareFuncs,
				OuterCompareFuncs:       outerCompareFuncs,
				Desc:                    !prop.IsEmpty() && prop.SortItems[0].Desc,
			}.Init(p.ctx)
			indexMergeJoins = append(indexMergeJoins, indexMergeJoin)
		}
	}
	return indexMergeJoins
}

func (p *LogicalJoin) constructIndexHashJoin(
	prop *property.PhysicalProperty,
	outerIdx int,
	innerTask task,
	ranges []*ranger.Range,
	keyOff2IdxOff []int,
	path *util.AccessPath,
	compareFilters *ColWithCmpFuncManager,
) []PhysicalPlan {
	indexJoins := p.constructIndexJoin(prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, compareFilters)
	indexHashJoins := make([]PhysicalPlan, 0, len(indexJoins))
	for _, plan := range indexJoins {
		join := plan.(*PhysicalIndexJoin)
		indexHashJoin := PhysicalIndexHashJoin{
			PhysicalIndexJoin: *join,
			// Prop is empty means that the parent operator does not need the
			// join operator to provide any promise of the output order.
			KeepOuterOrder: !prop.IsEmpty(),
		}.Init(p.ctx)
		indexHashJoins = append(indexHashJoins, indexHashJoin)
	}
	return indexHashJoins
}

// getIndexJoinByOuterIdx will generate index join by outerIndex. OuterIdx points out the outer child.
// First of all, we'll check whether the inner child is DataSource.
// Then, we will extract the join keys of p's equal conditions. Then check whether all of them are just the primary key
// or match some part of on index. If so we will choose the best one and construct a index join.
func (p *LogicalJoin) getIndexJoinByOuterIdx(prop *property.PhysicalProperty, outerIdx int) (joins []PhysicalPlan) {
	outerChild, innerChild := p.children[outerIdx], p.children[1-outerIdx]
	all, _ := prop.AllSameOrder()
	// If the order by columns are not all from outer child, index join cannot promise the order.
	if !prop.AllColsFromSchema(outerChild.Schema()) || !all {
		return nil
	}
	var (
		innerJoinKeys []*expression.Column
		outerJoinKeys []*expression.Column
	)
	if outerIdx == 0 {
		outerJoinKeys, innerJoinKeys, _, _ = p.GetJoinKeys()
	} else {
		innerJoinKeys, outerJoinKeys, _, _ = p.GetJoinKeys()
	}
	ds, isDataSource := innerChild.(*DataSource)
	us, isUnionScan := innerChild.(*LogicalUnionScan)
	if (!isDataSource && !isUnionScan) || (isDataSource && ds.preferStoreType&preferTiFlash != 0) {
		return nil
	}
	if isUnionScan {
		// The child of union scan may be union all for partition table.
		ds, isDataSource = us.Children()[0].(*DataSource)
		if !isDataSource {
			return nil
		}
		// If one of the union scan children is a TiFlash table, then we can't choose index join.
		for _, child := range us.Children() {
			if ds, ok := child.(*DataSource); ok && ds.preferStoreType&preferTiFlash != 0 {
				return nil
			}
		}
	}
	var avgInnerRowCnt float64
	if outerChild.statsInfo().RowCount > 0 {
		avgInnerRowCnt = p.equalCondOutCnt / outerChild.statsInfo().RowCount
	}
	joins = p.buildIndexJoinInner2TableScan(prop, ds, innerJoinKeys, outerJoinKeys, outerIdx, us, avgInnerRowCnt)
	if joins != nil {
		return
	}
	return p.buildIndexJoinInner2IndexScan(prop, ds, innerJoinKeys, outerJoinKeys, outerIdx, us, avgInnerRowCnt)
}

func (p *LogicalJoin) getIndexJoinBuildHelper(ds *DataSource, innerJoinKeys []*expression.Column, checkPathValid func(path *util.AccessPath) bool, outerJoinKeys []*expression.Column) (*indexJoinBuildHelper, []int) {
	helper := &indexJoinBuildHelper{
		join:      p,
		innerPlan: ds,
	}
	for _, path := range ds.possibleAccessPaths {
		if checkPathValid(path) {
			emptyRange, err := helper.analyzeLookUpFilters(path, ds, innerJoinKeys, outerJoinKeys)
			if emptyRange {
				return nil, nil
			}
			if err != nil {
				logutil.BgLogger().Warn("build index join failed", zap.Error(err))
			}
		}
	}
	if helper.chosenPath == nil {
		return nil, nil
	}
	keyOff2IdxOff := make([]int, len(innerJoinKeys))
	for i := range keyOff2IdxOff {
		keyOff2IdxOff[i] = -1
	}
	for idxOff, keyOff := range helper.idxOff2KeyOff {
		if keyOff != -1 {
			keyOff2IdxOff[keyOff] = idxOff
		}
	}
	return helper, keyOff2IdxOff
}

// buildIndexJoinInner2TableScan builds a TableScan as the inner child for an
// IndexJoin if possible.
// If the inner side of a index join is a TableScan, only one tuple will be
// fetched from the inner side for every tuple from the outer side. This will be
// promised to be no worse than building IndexScan as the inner child.
func (p *LogicalJoin) buildIndexJoinInner2TableScan(
	prop *property.PhysicalProperty, ds *DataSource, innerJoinKeys, outerJoinKeys []*expression.Column,
	outerIdx int, us *LogicalUnionScan, avgInnerRowCnt float64) (joins []PhysicalPlan) {
	var tblPath *util.AccessPath
	for _, path := range ds.possibleAccessPaths {
		if path.IsTablePath() && path.StoreType == kv.TiKV {
			tblPath = path
			break
		}
	}
	if tblPath == nil {
		return nil
	}
	keyOff2IdxOff := make([]int, len(innerJoinKeys))
	newOuterJoinKeys := make([]*expression.Column, 0)
	var ranges []*ranger.Range
	var innerTask, innerTask2 task
	var helper *indexJoinBuildHelper
	if ds.tableInfo.IsCommonHandle {
		helper, keyOff2IdxOff = p.getIndexJoinBuildHelper(ds, innerJoinKeys, func(path *util.AccessPath) bool { return path.IsCommonHandlePath }, outerJoinKeys)
		if helper == nil {
			return nil
		}
		innerTask = p.constructInnerTableScanTask(ds, nil, outerJoinKeys, us, false, false, avgInnerRowCnt)
		// The index merge join's inner plan is different from index join, so we
		// should construct another inner plan for it.
		// Because we can't keep order for union scan, if there is a union scan in inner task,
		// we can't construct index merge join.
		if us == nil {
			innerTask2 = p.constructInnerTableScanTask(ds, nil, outerJoinKeys, us, true, !prop.IsEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt)
		}
		ranges = helper.chosenRanges
	} else {
		pkMatched := false
		pkCol := ds.getPKIsHandleCol()
		if pkCol == nil {
			return nil
		}
		for i, key := range innerJoinKeys {
			if !key.Equal(nil, pkCol) {
				keyOff2IdxOff[i] = -1
				continue
			}
			pkMatched = true
			keyOff2IdxOff[i] = 0
			// Add to newOuterJoinKeys only if conditions contain inner primary key. For issue #14822.
			newOuterJoinKeys = append(newOuterJoinKeys, outerJoinKeys[i])
		}
		outerJoinKeys = newOuterJoinKeys
		if !pkMatched {
			return nil
		}
		innerTask = p.constructInnerTableScanTask(ds, pkCol, outerJoinKeys, us, false, false, avgInnerRowCnt)
		// The index merge join's inner plan is different from index join, so we
		// should construct another inner plan for it.
		// Because we can't keep order for union scan, if there is a union scan in inner task,
		// we can't construct index merge join.
		if us == nil {
			innerTask2 = p.constructInnerTableScanTask(ds, pkCol, outerJoinKeys, us, true, !prop.IsEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt)
		}
	}
	var (
		path       *util.AccessPath
		lastColMng *ColWithCmpFuncManager
	)
	if helper != nil {
		path = helper.chosenPath
		lastColMng = helper.lastColManager
	}
	joins = make([]PhysicalPlan, 0, 3)
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(p.constructIndexHashJoin(prop, outerIdx, innerTask, nil, keyOff2IdxOff, path, lastColMng))
		}
	})
	joins = append(joins, p.constructIndexJoin(prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, lastColMng)...)
	// We can reuse the `innerTask` here since index nested loop hash join
	// do not need the inner child to promise the order.
	joins = append(joins, p.constructIndexHashJoin(prop, outerIdx, innerTask, ranges, keyOff2IdxOff, path, lastColMng)...)
	if innerTask2 != nil {
		joins = append(joins, p.constructIndexMergeJoin(prop, outerIdx, innerTask2, ranges, keyOff2IdxOff, path, lastColMng)...)
	}
	return joins
}

func (p *LogicalJoin) buildIndexJoinInner2IndexScan(
	prop *property.PhysicalProperty, ds *DataSource, innerJoinKeys, outerJoinKeys []*expression.Column,
	outerIdx int, us *LogicalUnionScan, avgInnerRowCnt float64) (joins []PhysicalPlan) {
	helper, keyOff2IdxOff := p.getIndexJoinBuildHelper(ds, innerJoinKeys, func(path *util.AccessPath) bool { return !path.IsTablePath() }, outerJoinKeys)
	if helper == nil {
		return nil
	}
	joins = make([]PhysicalPlan, 0, 3)
	rangeInfo := helper.buildRangeDecidedByInformation(helper.chosenPath.IdxCols, outerJoinKeys)
	maxOneRow := false
	if helper.chosenPath.Index.Unique && helper.usedColsLen == len(helper.chosenPath.FullIdxCols) {
		l := len(helper.chosenAccess)
		if l == 0 {
			maxOneRow = true
		} else {
			sf, ok := helper.chosenAccess[l-1].(*expression.ScalarFunction)
			maxOneRow = ok && (sf.FuncName.L == ast.EQ)
		}
	}
	innerTask := p.constructInnerIndexScanTask(ds, helper.chosenPath, helper.chosenRemained, outerJoinKeys, us, rangeInfo, false, false, avgInnerRowCnt, maxOneRow)
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(p.constructIndexHashJoin(prop, outerIdx, innerTask, helper.chosenRanges, keyOff2IdxOff, helper.chosenPath, helper.lastColManager))
		}
	})
	joins = append(joins, p.constructIndexJoin(prop, outerIdx, innerTask, helper.chosenRanges, keyOff2IdxOff, helper.chosenPath, helper.lastColManager)...)
	// We can reuse the `innerTask` here since index nested loop hash join
	// do not need the inner child to promise the order.
	joins = append(joins, p.constructIndexHashJoin(prop, outerIdx, innerTask, helper.chosenRanges, keyOff2IdxOff, helper.chosenPath, helper.lastColManager)...)
	// The index merge join's inner plan is different from index join, so we
	// should construct another inner plan for it.
	// Because we can't keep order for union scan, if there is a union scan in inner task,
	// we can't construct index merge join.
	if us == nil {
		innerTask2 := p.constructInnerIndexScanTask(ds, helper.chosenPath, helper.chosenRemained, outerJoinKeys, us, rangeInfo, true, !prop.IsEmpty() && prop.SortItems[0].Desc, avgInnerRowCnt, maxOneRow)
		if innerTask2 != nil {
			joins = append(joins, p.constructIndexMergeJoin(prop, outerIdx, innerTask2, helper.chosenRanges, keyOff2IdxOff, helper.chosenPath, helper.lastColManager)...)
		}
	}
	return joins
}

type indexJoinBuildHelper struct {
	join      *LogicalJoin
	innerPlan *DataSource

	usedColsLen    int
	usedColsNDV    float64
	chosenAccess   []expression.Expression
	chosenRemained []expression.Expression
	idxOff2KeyOff  []int
	lastColManager *ColWithCmpFuncManager
	chosenRanges   []*ranger.Range
	chosenPath     *util.AccessPath

	curPossibleUsedKeys []*expression.Column
	curNotUsedIndexCols []*expression.Column
	curNotUsedColLens   []int
	curIdxOff2KeyOff    []int
}

func (ijHelper *indexJoinBuildHelper) buildRangeDecidedByInformation(idxCols []*expression.Column, outerJoinKeys []*expression.Column) string {
	buffer := bytes.NewBufferString("[")
	isFirst := true
	for idxOff, keyOff := range ijHelper.idxOff2KeyOff {
		if keyOff == -1 {
			continue
		}
		if !isFirst {
			buffer.WriteString(" ")
		} else {
			isFirst = false
		}
		buffer.WriteString(fmt.Sprintf("eq(%v, %v)", idxCols[idxOff], outerJoinKeys[keyOff]))
	}
	for _, access := range ijHelper.chosenAccess {
		if !isFirst {
			buffer.WriteString(" ")
		} else {
			isFirst = false
		}
		buffer.WriteString(fmt.Sprintf("%v", access))
	}
	buffer.WriteString("]")
	return buffer.String()
}

// constructInnerTableScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func (p *LogicalJoin) constructInnerTableScanTask(
	ds *DataSource,
	pk *expression.Column,
	outerJoinKeys []*expression.Column,
	us *LogicalUnionScan,
	keepOrder bool,
	desc bool,
	rowCount float64,
) task {
	// If `ds.tableInfo.GetPartitionInfo() != nil`,
	// it means the data source is a partition table reader.
	// If the inner task need to keep order, the partition table reader can't satisfy it.
	if keepOrder && ds.tableInfo.GetPartitionInfo() != nil {
		return nil
	}
	var ranges []*ranger.Range
	// pk is nil means the table uses the common handle.
	if pk == nil {
		ranges = ranger.FullRange()
	} else {
		ranges = ranger.FullIntRange(mysql.HasUnsignedFlag(pk.RetType.Flag))
	}
	ts := PhysicalTableScan{
		Table:           ds.tableInfo,
		Columns:         ds.Columns,
		TableAsName:     ds.TableAsName,
		DBName:          ds.DBName,
		filterCondition: ds.pushedDownConds,
		Ranges:          ranges,
		rangeDecidedBy:  outerJoinKeys,
		KeepOrder:       keepOrder,
		Desc:            desc,
		physicalTableID: ds.physicalTableID,
		isPartition:     ds.isPartition,
	}.Init(ds.ctx, ds.blockOffset)
	ts.SetSchema(ds.schema.Clone())
	if rowCount <= 0 {
		rowCount = float64(1)
	}
	selectivity := float64(1)
	countAfterAccess := rowCount
	if len(ts.filterCondition) > 0 {
		var err error
		selectivity, _, err = ds.tableStats.HistColl.Selectivity(ds.ctx, ts.filterCondition, ds.possibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ts.TableAsName.L))
			selectivity = SelectionFactor
		}
		// rowCount is computed from result row count of join, which has already accounted the filters on DataSource,
		// i.e, rowCount equals to `countAfterAccess * selectivity`.
		countAfterAccess = rowCount / selectivity
	}
	ts.stats = &property.StatsInfo{
		// TableScan as inner child of IndexJoin can return at most 1 tuple for each outer row.
		RowCount:     math.Min(1.0, countAfterAccess),
		StatsVersion: ds.stats.StatsVersion,
		// Cardinality would not be used in cost computation of IndexJoin, set leave it as default nil.
	}
	rowSize := ds.TblColHists.GetTableAvgRowSize(p.ctx, ds.TblCols, ts.StoreType, true)
	sessVars := ds.ctx.GetSessionVars()
	copTask := &copTask{
		tablePlan:         ts,
		indexPlanFinished: true,
		cst:               sessVars.GetScanFactor(ts.Table) * rowSize * ts.stats.RowCount,
		tblColHists:       ds.TblColHists,
		keepOrder:         ts.KeepOrder,
	}
	copTask.partitionInfo = PartitionInfo{
		PruningConds:   ds.allConds,
		PartitionNames: ds.partitionNames,
		Columns:        ds.TblCols,
		ColumnNames:    ds.names,
	}
	selStats := ts.stats.Scale(selectivity)
	ts.addPushedDownSelection(copTask, selStats)
	t := copTask.convertToRootTask(ds.ctx)
	reader := t.p
	t.p = p.constructInnerUnionScan(us, reader)
	return t
}

func (p *LogicalJoin) constructInnerUnionScan(us *LogicalUnionScan, reader PhysicalPlan) PhysicalPlan {
	if us == nil {
		return reader
	}
	// Use `reader.stats` instead of `us.stats` because it should be more accurate. No need to specify
	// childrenReqProps now since we have got reader already.
	physicalUnionScan := PhysicalUnionScan{
		Conditions: us.conditions,
		HandleCols: us.handleCols,
	}.Init(us.ctx, reader.statsInfo(), us.blockOffset, nil)
	physicalUnionScan.SetChildren(reader)
	return physicalUnionScan
}

// constructInnerIndexScanTask is specially used to construct the inner plan for PhysicalIndexJoin.
func (p *LogicalJoin) constructInnerIndexScanTask(
	ds *DataSource,
	path *util.AccessPath,
	filterConds []expression.Expression,
	outerJoinKeys []*expression.Column,
	us *LogicalUnionScan,
	rangeInfo string,
	keepOrder bool,
	desc bool,
	rowCount float64,
	maxOneRow bool,
) task {
	// If `ds.tableInfo.GetPartitionInfo() != nil`,
	// it means the data source is a partition table reader.
	// If the inner task need to keep order, the partition table reader can't satisfy it.
	if keepOrder && ds.tableInfo.GetPartitionInfo() != nil {
		return nil
	}
	is := PhysicalIndexScan{
		Table:            ds.tableInfo,
		TableAsName:      ds.TableAsName,
		DBName:           ds.DBName,
		Columns:          ds.Columns,
		Index:            path.Index,
		IdxCols:          path.IdxCols,
		IdxColLens:       path.IdxColLens,
		dataSourceSchema: ds.schema,
		KeepOrder:        keepOrder,
		Ranges:           ranger.FullRange(),
		rangeInfo:        rangeInfo,
		Desc:             desc,
		isPartition:      ds.isPartition,
		physicalTableID:  ds.physicalTableID,
	}.Init(ds.ctx, ds.blockOffset)
	cop := &copTask{
		indexPlan:   is,
		tblColHists: ds.TblColHists,
		tblCols:     ds.TblCols,
		keepOrder:   is.KeepOrder,
	}
	cop.partitionInfo = PartitionInfo{
		PruningConds:   ds.allConds,
		PartitionNames: ds.partitionNames,
		Columns:        ds.TblCols,
		ColumnNames:    ds.names,
	}
	if !ds.isCoveringIndex(ds.schema.Columns, path.FullIdxCols, path.FullIdxColLens, is.Table) {
		// On this way, it's double read case.
		ts := PhysicalTableScan{
			Columns:         ds.Columns,
			Table:           is.Table,
			TableAsName:     ds.TableAsName,
			isPartition:     ds.isPartition,
			physicalTableID: ds.physicalTableID,
		}.Init(ds.ctx, ds.blockOffset)
		ts.schema = is.dataSourceSchema.Clone()
		if ds.tableInfo.IsCommonHandle {
			commonHandle := ds.handleCols.(*CommonHandleCols)
			for _, col := range commonHandle.columns {
				if ts.schema.ColumnIndex(col) == -1 {
					ts.Schema().Append(col)
					ts.Columns = append(ts.Columns, col.ToInfo())
					cop.needExtraProj = true
				}
			}
		}
		// If inner cop task need keep order, the extraHandleCol should be set.
		if cop.keepOrder && !ds.tableInfo.IsCommonHandle {
			var needExtraProj bool
			cop.extraHandleCol, needExtraProj = ts.appendExtraHandleCol(ds)
			cop.needExtraProj = cop.needExtraProj || needExtraProj
		}
		if cop.needExtraProj {
			cop.originSchema = ds.schema
		}
		cop.tablePlan = ts
	}
	if cop.tablePlan != nil && ds.tableInfo.IsCommonHandle {
		cop.commonHandleCols = ds.commonHandleCols
	}
	is.initSchema(append(path.FullIdxCols, ds.commonHandleCols...), cop.tablePlan != nil)
	indexConds, tblConds := ds.splitIndexFilterConditions(filterConds, path.FullIdxCols, path.FullIdxColLens, ds.tableInfo)
	if maxOneRow {
		// Theoretically, this line is unnecessary because row count estimation of join should guarantee rowCount is not larger
		// than 1.0; however, there may be rowCount larger than 1.0 in reality, e.g, pseudo statistics cases, which does not reflect
		// unique constraint in NDV.
		rowCount = math.Min(rowCount, 1.0)
	}
	tmpPath := &util.AccessPath{
		IndexFilters:     indexConds,
		TableFilters:     tblConds,
		CountAfterIndex:  rowCount,
		CountAfterAccess: rowCount,
	}
	// Assume equal conditions used by index join and other conditions are independent.
	if len(tblConds) > 0 {
		selectivity, _, err := ds.tableStats.HistColl.Selectivity(ds.ctx, tblConds, ds.possibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ds.TableAsName.L))
			selectivity = SelectionFactor
		}
		// rowCount is computed from result row count of join, which has already accounted the filters on DataSource,
		// i.e, rowCount equals to `countAfterIndex * selectivity`.
		cnt := rowCount / selectivity
		if maxOneRow {
			cnt = math.Min(cnt, 1.0)
		}
		tmpPath.CountAfterIndex = cnt
		tmpPath.CountAfterAccess = cnt
	}
	if len(indexConds) > 0 {
		selectivity, _, err := ds.tableStats.HistColl.Selectivity(ds.ctx, indexConds, ds.possibleAccessPaths)
		if err != nil || selectivity <= 0 {
			logutil.BgLogger().Debug("unexpected selectivity, use selection factor", zap.Float64("selectivity", selectivity), zap.String("table", ds.TableAsName.L))
			selectivity = SelectionFactor
		}
		cnt := tmpPath.CountAfterIndex / selectivity
		if maxOneRow {
			cnt = math.Min(cnt, 1.0)
		}
		tmpPath.CountAfterAccess = cnt
	}
	is.stats = ds.tableStats.ScaleByExpectCnt(tmpPath.CountAfterAccess)
	rowSize := is.indexScanRowSize(path.Index, ds, true)
	sessVars := ds.ctx.GetSessionVars()
	cop.cst = tmpPath.CountAfterAccess * rowSize * sessVars.GetScanFactor(ds.tableInfo)
	finalStats := ds.tableStats.ScaleByExpectCnt(rowCount)
	is.addPushedDownSelection(cop, ds, tmpPath, finalStats)
	t := cop.convertToRootTask(ds.ctx)
	reader := t.p
	t.p = p.constructInnerUnionScan(us, reader)
	return t
}

var symmetricOp = map[string]string{
	ast.LT: ast.GT,
	ast.GE: ast.LE,
	ast.GT: ast.LT,
	ast.LE: ast.GE,
}

// ColWithCmpFuncManager is used in index join to handle the column with compare functions(>=, >, <, <=).
// It stores the compare functions and build ranges in execution phase.
type ColWithCmpFuncManager struct {
	TargetCol         *expression.Column
	colLength         int
	OpType            []string
	opArg             []expression.Expression
	TmpConstant       []*expression.Constant
	affectedColSchema *expression.Schema
	compareFuncs      []chunk.CompareFunc
}

func (cwc *ColWithCmpFuncManager) appendNewExpr(opName string, arg expression.Expression, affectedCols []*expression.Column) {
	cwc.OpType = append(cwc.OpType, opName)
	cwc.opArg = append(cwc.opArg, arg)
	cwc.TmpConstant = append(cwc.TmpConstant, &expression.Constant{RetType: cwc.TargetCol.RetType})
	for _, col := range affectedCols {
		if cwc.affectedColSchema.Contains(col) {
			continue
		}
		cwc.compareFuncs = append(cwc.compareFuncs, chunk.GetCompareFunc(col.RetType))
		cwc.affectedColSchema.Append(col)
	}
}

// CompareRow compares the rows for deduplicate.
func (cwc *ColWithCmpFuncManager) CompareRow(lhs, rhs chunk.Row) int {
	for i, col := range cwc.affectedColSchema.Columns {
		ret := cwc.compareFuncs[i](lhs, col.Index, rhs, col.Index)
		if ret != 0 {
			return ret
		}
	}
	return 0
}

// BuildRangesByRow will build range of the given row. It will eval each function's arg then call BuildRange.
func (cwc *ColWithCmpFuncManager) BuildRangesByRow(ctx sessionctx.Context, row chunk.Row) ([]*ranger.Range, error) {
	exprs := make([]expression.Expression, len(cwc.OpType))
	for i, opType := range cwc.OpType {
		constantArg, err := cwc.opArg[i].Eval(row)
		if err != nil {
			return nil, err
		}
		cwc.TmpConstant[i].Value = constantArg
		newExpr, err := expression.NewFunction(ctx, opType, types.NewFieldType(mysql.TypeTiny), cwc.TargetCol, cwc.TmpConstant[i])
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, newExpr)
	}
	ranges, err := ranger.BuildColumnRange(exprs, ctx.GetSessionVars().StmtCtx, cwc.TargetCol.RetType, cwc.colLength)
	if err != nil {
		return nil, err
	}
	return ranges, nil
}

func (cwc *ColWithCmpFuncManager) resolveIndices(schema *expression.Schema) (err error) {
	for i := range cwc.opArg {
		cwc.opArg[i], err = cwc.opArg[i].ResolveIndices(schema)
		if err != nil {
			return err
		}
	}
	return nil
}

// String implements Stringer interface.
func (cwc *ColWithCmpFuncManager) String() string {
	buffer := bytes.NewBufferString("")
	for i := range cwc.OpType {
		buffer.WriteString(fmt.Sprintf("%v(%v, %v)", cwc.OpType[i], cwc.TargetCol, cwc.opArg[i]))
		if i < len(cwc.OpType)-1 {
			buffer.WriteString(" ")
		}
	}
	return buffer.String()
}

func (ijHelper *indexJoinBuildHelper) resetContextForIndex(innerKeys []*expression.Column, idxCols []*expression.Column, colLens []int, outerKeys []*expression.Column) {
	tmpSchema := expression.NewSchema(innerKeys...)
	ijHelper.curIdxOff2KeyOff = make([]int, len(idxCols))
	ijHelper.curNotUsedIndexCols = make([]*expression.Column, 0, len(idxCols))
	ijHelper.curNotUsedColLens = make([]int, 0, len(idxCols))
	for i, idxCol := range idxCols {
		ijHelper.curIdxOff2KeyOff[i] = tmpSchema.ColumnIndex(idxCol)
		if ijHelper.curIdxOff2KeyOff[i] >= 0 {
			// Don't use the join columns if their collations are unmatched and the new collation is enabled.
			if collate.NewCollationEnabled() && types.IsString(idxCol.RetType.Tp) && types.IsString(outerKeys[ijHelper.curIdxOff2KeyOff[i]].RetType.Tp) {
				_, coll := expression.DeriveCollationFromExprs(nil, idxCol, outerKeys[ijHelper.curIdxOff2KeyOff[i]])
				if !collate.CompatibleCollate(idxCol.GetType().Collate, coll) {
					ijHelper.curIdxOff2KeyOff[i] = -1
				}
			}
			continue
		}
		ijHelper.curNotUsedIndexCols = append(ijHelper.curNotUsedIndexCols, idxCol)
		ijHelper.curNotUsedColLens = append(ijHelper.curNotUsedColLens, colLens[i])
	}
}

// findUsefulEqAndInFilters analyzes the pushedDownConds held by inner child and split them to three parts.
// usefulEqOrInFilters is the continuous eq/in conditions on current unused index columns.
// uselessFilters is the conditions which cannot be used for building ranges.
// remainingRangeCandidates is the other conditions for future use.
func (ijHelper *indexJoinBuildHelper) findUsefulEqAndInFilters(innerPlan *DataSource) (usefulEqOrInFilters, uselessFilters, remainingRangeCandidates []expression.Expression) {
	uselessFilters = make([]expression.Expression, 0, len(innerPlan.pushedDownConds))
	var remainedEqOrIn []expression.Expression
	// Extract the eq/in functions of possible join key.
	// you can see the comment of ExtractEqAndInCondition to get the meaning of the second return value.
	usefulEqOrInFilters, remainedEqOrIn, remainingRangeCandidates, _ = ranger.ExtractEqAndInCondition(
		innerPlan.ctx, innerPlan.pushedDownConds,
		ijHelper.curNotUsedIndexCols,
		ijHelper.curNotUsedColLens,
	)
	uselessFilters = append(uselessFilters, remainedEqOrIn...)
	return usefulEqOrInFilters, uselessFilters, remainingRangeCandidates
}

// buildLastColManager analyze the `OtherConditions` of join to see whether there're some filters can be used in manager.
// The returned value is just for outputting explain information
func (ijHelper *indexJoinBuildHelper) buildLastColManager(nextCol *expression.Column,
	innerPlan *DataSource, cwc *ColWithCmpFuncManager) []expression.Expression {
	var lastColAccesses []expression.Expression
loopOtherConds:
	for _, filter := range ijHelper.join.OtherConditions {
		sf, ok := filter.(*expression.ScalarFunction)
		if !ok || !(sf.FuncName.L == ast.LE || sf.FuncName.L == ast.LT || sf.FuncName.L == ast.GE || sf.FuncName.L == ast.GT) {
			continue
		}
		var funcName string
		var anotherArg expression.Expression
		if lCol, ok := sf.GetArgs()[0].(*expression.Column); ok && lCol.Equal(nil, nextCol) {
			anotherArg = sf.GetArgs()[1]
			funcName = sf.FuncName.L
		} else if rCol, ok := sf.GetArgs()[1].(*expression.Column); ok && rCol.Equal(nil, nextCol) {
			anotherArg = sf.GetArgs()[0]
			// The column manager always build expression in the form of col op arg1.
			// So we need use the symmetric one of the current function.
			funcName = symmetricOp[sf.FuncName.L]
		} else {
			continue
		}
		affectedCols := expression.ExtractColumns(anotherArg)
		if len(affectedCols) == 0 {
			continue
		}
		for _, col := range affectedCols {
			if innerPlan.schema.Contains(col) {
				continue loopOtherConds
			}
		}
		lastColAccesses = append(lastColAccesses, sf)
		cwc.appendNewExpr(funcName, anotherArg, affectedCols)
	}
	return lastColAccesses
}

// removeUselessEqAndInFunc removes the useless eq/in conditions. It's designed for the following case:
//   t1 join t2 on t1.a=t2.a and t1.c=t2.c where t1.b > t2.b-10 and t1.b < t2.b+10 there's index(a, b, c) on t1.
//   In this case the curIdxOff2KeyOff is [0 -1 1] and the notKeyEqAndIn is [].
//   It's clearly that the column c cannot be used to access data. So we need to remove it and reset the IdxOff2KeyOff to
//   [0 -1 -1].
//   So that we can use t1.a=t2.a and t1.b > t2.b-10 and t1.b < t2.b+10 to build ranges then access data.
func (ijHelper *indexJoinBuildHelper) removeUselessEqAndInFunc(idxCols []*expression.Column, notKeyEqAndIn []expression.Expression, outerJoinKeys []*expression.Column) (usefulEqAndIn, uselessOnes []expression.Expression) {
	ijHelper.curPossibleUsedKeys = make([]*expression.Column, 0, len(idxCols))
	for idxColPos, notKeyColPos := 0, 0; idxColPos < len(idxCols); idxColPos++ {
		if ijHelper.curIdxOff2KeyOff[idxColPos] != -1 {
			ijHelper.curPossibleUsedKeys = append(ijHelper.curPossibleUsedKeys, idxCols[idxColPos])
			continue
		}
		if notKeyColPos < len(notKeyEqAndIn) && ijHelper.curNotUsedIndexCols[notKeyColPos].Equal(nil, idxCols[idxColPos]) {
			notKeyColPos++
			continue
		}
		for i := idxColPos + 1; i < len(idxCols); i++ {
			ijHelper.curIdxOff2KeyOff[i] = -1
		}
		remained := make([]expression.Expression, 0, len(notKeyEqAndIn)-notKeyColPos)
		remained = append(remained, notKeyEqAndIn[notKeyColPos:]...)
		notKeyEqAndIn = notKeyEqAndIn[:notKeyColPos]
		return notKeyEqAndIn, remained
	}
	return notKeyEqAndIn, nil
}

func (ijHelper *indexJoinBuildHelper) analyzeLookUpFilters(path *util.AccessPath, innerPlan *DataSource, innerJoinKeys []*expression.Column, outerJoinKeys []*expression.Column) (emptyRange bool, err error) {
	if len(path.IdxCols) == 0 {
		return false, nil
	}
	accesses := make([]expression.Expression, 0, len(path.IdxCols))
	ijHelper.resetContextForIndex(innerJoinKeys, path.IdxCols, path.IdxColLens, outerJoinKeys)
	notKeyEqAndIn, remained, rangeFilterCandidates := ijHelper.findUsefulEqAndInFilters(innerPlan)
	var remainedEqAndIn []expression.Expression
	notKeyEqAndIn, remainedEqAndIn = ijHelper.removeUselessEqAndInFunc(path.IdxCols, notKeyEqAndIn, outerJoinKeys)
	matchedKeyCnt := len(ijHelper.curPossibleUsedKeys)
	// If no join key is matched while join keys actually are not empty. We don't choose index join for now.
	if matchedKeyCnt <= 0 && len(innerJoinKeys) > 0 {
		return false, nil
	}
	accesses = append(accesses, notKeyEqAndIn...)
	remained = append(remained, remainedEqAndIn...)
	lastColPos := matchedKeyCnt + len(notKeyEqAndIn)
	// There should be some equal conditions. But we don't need that there must be some join key in accesses here.
	// A more strict check is applied later.
	if lastColPos <= 0 {
		return false, nil
	}
	// If all the index columns are covered by eq/in conditions, we don't need to consider other conditions anymore.
	if lastColPos == len(path.IdxCols) {
		// If there's join key matched index column. Then choose hash join is always a better idea.
		// e.g. select * from t1, t2 where t2.a=1 and t2.b=1. And t2 has index(a, b).
		//      If we don't have the following check, TiDB will build index join for this case.
		if matchedKeyCnt <= 0 {
			return false, nil
		}
		remained = append(remained, rangeFilterCandidates...)
		ranges, emptyRange, err := ijHelper.buildTemplateRange(matchedKeyCnt, notKeyEqAndIn, nil, false)
		if err != nil {
			return false, err
		}
		if emptyRange {
			return true, nil
		}
		ijHelper.updateBestChoice(ranges, path, accesses, remained, nil, lastColPos)
		return false, nil
	}
	lastPossibleCol := path.IdxCols[lastColPos]
	lastColManager := &ColWithCmpFuncManager{
		TargetCol:         lastPossibleCol,
		colLength:         path.IdxColLens[lastColPos],
		affectedColSchema: expression.NewSchema(),
	}
	lastColAccess := ijHelper.buildLastColManager(lastPossibleCol, innerPlan, lastColManager)
	// If the column manager holds no expression, then we fallback to find whether there're useful normal filters
	if len(lastColAccess) == 0 {
		// If there's join key matched index column. Then choose hash join is always a better idea.
		// e.g. select * from t1, t2 where t2.a=1 and t2.b=1 and t2.c > 10 and t2.c < 20. And t2 has index(a, b, c).
		//      If we don't have the following check, TiDB will build index join for this case.
		if matchedKeyCnt <= 0 {
			return false, nil
		}
		colAccesses, colRemained := ranger.DetachCondsForColumn(ijHelper.join.ctx, rangeFilterCandidates, lastPossibleCol)
		var ranges, nextColRange []*ranger.Range
		var err error
		if len(colAccesses) > 0 {
			nextColRange, err = ranger.BuildColumnRange(colAccesses, ijHelper.join.ctx.GetSessionVars().StmtCtx, lastPossibleCol.RetType, path.IdxColLens[lastColPos])
			if err != nil {
				return false, err
			}
		}
		ranges, emptyRange, err = ijHelper.buildTemplateRange(matchedKeyCnt, notKeyEqAndIn, nextColRange, false)
		if err != nil {
			return false, err
		}
		if emptyRange {
			return true, nil
		}
		remained = append(remained, colRemained...)
		if path.IdxColLens[lastColPos] != types.UnspecifiedLength {
			remained = append(remained, colAccesses...)
		}
		accesses = append(accesses, colAccesses...)
		if len(colAccesses) > 0 {
			lastColPos = lastColPos + 1
		}
		ijHelper.updateBestChoice(ranges, path, accesses, remained, nil, lastColPos)
		return false, nil
	}
	accesses = append(accesses, lastColAccess...)
	remained = append(remained, rangeFilterCandidates...)
	ranges, emptyRange, err := ijHelper.buildTemplateRange(matchedKeyCnt, notKeyEqAndIn, nil, true)
	if err != nil {
		return false, err
	}
	if emptyRange {
		return true, nil
	}
	ijHelper.updateBestChoice(ranges, path, accesses, remained, lastColManager, lastColPos+1)
	return false, nil
}

func (ijHelper *indexJoinBuildHelper) updateBestChoice(ranges []*ranger.Range, path *util.AccessPath, accesses,
	remained []expression.Expression, lastColManager *ColWithCmpFuncManager, usedColsLen int) {
	// Notice that there may be the cases like `t1.a = t2.a and b > 2 and b < 1`, so ranges can be nil though the conditions are valid.
	// Obviously when the range is nil, we don't need index join.
	if len(ranges) == 0 {
		return
	}
	var innerNDV float64
	if stats := ijHelper.innerPlan.statsInfo(); stats != nil && stats.StatsVersion != statistics.PseudoVersion {
		innerNDV = getCardinality(path.IdxCols[:usedColsLen], ijHelper.innerPlan.Schema(), stats)
	}
	// We choose the index by the NDV of the used columns, the larger the better.
	// If NDVs are same, we choose index which uses more columns.
	// Note that these 2 heuristic rules are too simple to cover all cases,
	// since the NDV of outer join keys are not considered, and the detached access conditions
	// may contain expressions like `t1.a > t2.a`. It's pretty hard to evaluate the join selectivity
	// of these non-column-equal conditions, so I prefer to keep these heuristic rules simple at least for now.
	if innerNDV < ijHelper.usedColsNDV || (innerNDV == ijHelper.usedColsNDV && usedColsLen <= ijHelper.usedColsLen) {
		return
	}
	ijHelper.chosenPath = path
	ijHelper.usedColsLen = len(ranges[0].LowVal)
	ijHelper.usedColsNDV = innerNDV
	ijHelper.chosenRanges = ranges
	ijHelper.chosenAccess = accesses
	ijHelper.chosenRemained = remained
	ijHelper.idxOff2KeyOff = ijHelper.curIdxOff2KeyOff
	ijHelper.lastColManager = lastColManager
}

func (ijHelper *indexJoinBuildHelper) buildTemplateRange(matchedKeyCnt int, eqAndInFuncs []expression.Expression, nextColRange []*ranger.Range, haveExtraCol bool) (ranges []*ranger.Range, emptyRange bool, err error) {
	pointLength := matchedKeyCnt + len(eqAndInFuncs)
	//nolint:gosimple // false positive unnecessary nil check
	if nextColRange != nil {
		for _, colRan := range nextColRange {
			// The range's exclude status is the same with last col's.
			ran := &ranger.Range{
				LowVal:      make([]types.Datum, pointLength, pointLength+1),
				HighVal:     make([]types.Datum, pointLength, pointLength+1),
				LowExclude:  colRan.LowExclude,
				HighExclude: colRan.HighExclude,
			}
			ran.LowVal = append(ran.LowVal, colRan.LowVal[0])
			ran.HighVal = append(ran.HighVal, colRan.HighVal[0])
			ranges = append(ranges, ran)
		}
	} else if haveExtraCol {
		// Reserve a position for the last col.
		ranges = append(ranges, &ranger.Range{
			LowVal:  make([]types.Datum, pointLength+1),
			HighVal: make([]types.Datum, pointLength+1),
		})
	} else {
		ranges = append(ranges, &ranger.Range{
			LowVal:  make([]types.Datum, pointLength),
			HighVal: make([]types.Datum, pointLength),
		})
	}
	sc := ijHelper.join.ctx.GetSessionVars().StmtCtx
	for i, j := 0, 0; j < len(eqAndInFuncs); i++ {
		// This position is occupied by join key.
		if ijHelper.curIdxOff2KeyOff[i] != -1 {
			continue
		}
		oneColumnRan, err := ranger.BuildColumnRange([]expression.Expression{eqAndInFuncs[j]}, sc, ijHelper.curNotUsedIndexCols[j].RetType, ijHelper.curNotUsedColLens[j])
		if err != nil {
			return nil, false, err
		}
		if len(oneColumnRan) == 0 {
			return nil, true, nil
		}
		for _, ran := range ranges {
			ran.LowVal[i] = oneColumnRan[0].LowVal[0]
			ran.HighVal[i] = oneColumnRan[0].HighVal[0]
		}
		curRangeLen := len(ranges)
		for ranIdx := 1; ranIdx < len(oneColumnRan); ranIdx++ {
			newRanges := make([]*ranger.Range, 0, curRangeLen)
			for oldRangeIdx := 0; oldRangeIdx < curRangeLen; oldRangeIdx++ {
				newRange := ranges[oldRangeIdx].Clone()
				newRange.LowVal[i] = oneColumnRan[ranIdx].LowVal[0]
				newRange.HighVal[i] = oneColumnRan[ranIdx].HighVal[0]
				newRanges = append(newRanges, newRange)
			}
			ranges = append(ranges, newRanges...)
		}
		j++
	}
	return ranges, false, nil
}

func filterIndexJoinBySessionVars(sc sessionctx.Context, indexJoins []PhysicalPlan) []PhysicalPlan {
	if sc.GetSessionVars().EnableIndexMergeJoin {
		return indexJoins
	}
	for i := len(indexJoins) - 1; i >= 0; i-- {
		if _, ok := indexJoins[i].(*PhysicalIndexMergeJoin); ok {
			indexJoins = append(indexJoins[:i], indexJoins[i+1:]...)
		}
	}
	return indexJoins
}

// tryToGetIndexJoin will get index join by hints. If we can generate a valid index join by hint, the second return value
// will be true, which means we force to choose this index join. Otherwise we will select a join algorithm with min-cost.
func (p *LogicalJoin) tryToGetIndexJoin(prop *property.PhysicalProperty) (indexJoins []PhysicalPlan, canForced bool) {
	inljRightOuter := (p.preferJoinType & preferLeftAsINLJInner) > 0
	inljLeftOuter := (p.preferJoinType & preferRightAsINLJInner) > 0
	hasINLJHint := inljLeftOuter || inljRightOuter

	inlhjRightOuter := (p.preferJoinType & preferLeftAsINLHJInner) > 0
	inlhjLeftOuter := (p.preferJoinType & preferRightAsINLHJInner) > 0
	hasINLHJHint := inlhjLeftOuter || inlhjRightOuter

	inlmjRightOuter := (p.preferJoinType & preferLeftAsINLMJInner) > 0
	inlmjLeftOuter := (p.preferJoinType & preferRightAsINLMJInner) > 0
	hasINLMJHint := inlmjLeftOuter || inlmjRightOuter

	forceLeftOuter := inljLeftOuter || inlhjLeftOuter || inlmjLeftOuter
	forceRightOuter := inljRightOuter || inlhjRightOuter || inlmjRightOuter
	needForced := forceLeftOuter || forceRightOuter

	defer func() {
		// refine error message
		// If the required property is not empty, we will enforce it and try the hint again.
		// So we only need to generate warning message when the property is empty.
		if !canForced && needForced && prop.IsEmpty() {
			// Construct warning message prefix.
			var errMsg string
			switch {
			case hasINLJHint:
				errMsg = "Optimizer Hint INL_JOIN or TIDB_INLJ is inapplicable"
			case hasINLHJHint:
				errMsg = "Optimizer Hint INL_HASH_JOIN is inapplicable"
			case hasINLMJHint:
				errMsg = "Optimizer Hint INL_MERGE_JOIN is inapplicable"
			}
			if p.hintInfo != nil {
				t := p.hintInfo.indexNestedLoopJoinTables
				switch {
				case len(t.inljTables) != 0:
					errMsg = fmt.Sprintf("Optimizer Hint %s or %s is inapplicable",
						restore2JoinHint(HintINLJ, t.inljTables), restore2JoinHint(TiDBIndexNestedLoopJoin, t.inljTables))
				case len(t.inlhjTables) != 0:
					errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", restore2JoinHint(HintINLHJ, t.inlhjTables))
				case len(t.inlmjTables) != 0:
					errMsg = fmt.Sprintf("Optimizer Hint %s is inapplicable", restore2JoinHint(HintINLMJ, t.inlmjTables))
				}
			}

			// Append inapplicable reason.
			if len(p.EqualConditions) == 0 {
				errMsg += " without column equal ON condition"
			}

			// Generate warning message to client.
			warning := ErrInternal.GenWithStack(errMsg)
			p.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
		}
	}()

	// supportLeftOuter and supportRightOuter indicates whether this type of join
	// supports the left side or right side to be the outer side.
	var supportLeftOuter, supportRightOuter bool
	switch p.JoinType {
	case SemiJoin, AntiSemiJoin, LeftOuterSemiJoin, AntiLeftOuterSemiJoin, LeftOuterJoin:
		supportLeftOuter = true
	case RightOuterJoin:
		supportRightOuter = true
	case InnerJoin:
		supportLeftOuter, supportRightOuter = true, true
	}

	var allLeftOuterJoins, allRightOuterJoins, forcedLeftOuterJoins, forcedRightOuterJoins []PhysicalPlan
	if supportLeftOuter {
		allLeftOuterJoins = p.getIndexJoinByOuterIdx(prop, 0)
		forcedLeftOuterJoins = make([]PhysicalPlan, 0, len(allLeftOuterJoins))
		for _, j := range allLeftOuterJoins {
			switch j.(type) {
			case *PhysicalIndexJoin:
				if inljLeftOuter {
					forcedLeftOuterJoins = append(forcedLeftOuterJoins, j)
				}
			case *PhysicalIndexHashJoin:
				if inlhjLeftOuter {
					forcedLeftOuterJoins = append(forcedLeftOuterJoins, j)
				}
			case *PhysicalIndexMergeJoin:
				if inlmjLeftOuter {
					forcedLeftOuterJoins = append(forcedLeftOuterJoins, j)
				}
			}
		}
		switch {
		case len(forcedLeftOuterJoins) == 0 && !supportRightOuter:
			return filterIndexJoinBySessionVars(p.ctx, allLeftOuterJoins), false
		case len(forcedLeftOuterJoins) != 0 && (!supportRightOuter || (forceLeftOuter && !forceRightOuter)):
			return forcedLeftOuterJoins, true
		}
	}
	if supportRightOuter {
		allRightOuterJoins = p.getIndexJoinByOuterIdx(prop, 1)
		forcedRightOuterJoins = make([]PhysicalPlan, 0, len(allRightOuterJoins))
		for _, j := range allRightOuterJoins {
			switch j.(type) {
			case *PhysicalIndexJoin:
				if inljRightOuter {
					forcedRightOuterJoins = append(forcedRightOuterJoins, j)
				}
			case *PhysicalIndexHashJoin:
				if inlhjRightOuter {
					forcedRightOuterJoins = append(forcedRightOuterJoins, j)
				}
			case *PhysicalIndexMergeJoin:
				if inlmjRightOuter {
					forcedRightOuterJoins = append(forcedRightOuterJoins, j)
				}
			}
		}
		switch {
		case len(forcedRightOuterJoins) == 0 && !supportLeftOuter:
			return filterIndexJoinBySessionVars(p.ctx, allRightOuterJoins), false
		case len(forcedRightOuterJoins) != 0 && (!supportLeftOuter || (forceRightOuter && !forceLeftOuter)):
			return forcedRightOuterJoins, true
		}
	}

	canForceLeft := len(forcedLeftOuterJoins) != 0 && forceLeftOuter
	canForceRight := len(forcedRightOuterJoins) != 0 && forceRightOuter
	canForced = canForceLeft || canForceRight
	if canForced {
		return append(forcedLeftOuterJoins, forcedRightOuterJoins...), true
	}
	return filterIndexJoinBySessionVars(p.ctx, append(allLeftOuterJoins, allRightOuterJoins...)), false
}

func checkChildFitBC(p Plan) bool {
	if p.statsInfo().HistColl == nil {
		return p.statsInfo().Count() < p.SCtx().GetSessionVars().BroadcastJoinThresholdCount
	}
	avg := p.statsInfo().HistColl.GetAvgRowSize(p.SCtx(), p.Schema().Columns, false, false)
	sz := avg * float64(p.statsInfo().Count())
	return sz < float64(p.SCtx().GetSessionVars().BroadcastJoinThresholdSize)
}

// If we can use mpp broadcast join, that's our first choice.

func (p *LogicalJoin) shouldUseMPPBCJ() bool {
	if p.ctx.GetSessionVars().BroadcastJoinThresholdSize == 0 || p.ctx.GetSessionVars().BroadcastJoinThresholdCount == 0 {
		return p.ctx.GetSessionVars().AllowBCJ
	}
	if len(p.EqualConditions) == 0 && p.ctx.GetSessionVars().AllowCartesianBCJ == 2 {
		return true
	}
	if p.JoinType == LeftOuterJoin || p.JoinType == SemiJoin || p.JoinType == AntiSemiJoin {
		return checkChildFitBC(p.children[1])
	} else if p.JoinType == RightOuterJoin {
		return checkChildFitBC(p.children[0])
	}
	return checkChildFitBC(p.children[0]) || checkChildFitBC(p.children[1])
}

// LogicalJoin can generates hash join, index join and sort merge join.
// Firstly we check the hint, if hint is figured by user, we force to choose the corresponding physical plan.
// If the hint is not matched, it will get other candidates.
// If the hint is not figured, we will pick all candidates.
func (p *LogicalJoin) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	failpoint.Inject("MockOnlyEnableIndexHashJoin", func(val failpoint.Value) {
		if val.(bool) {
			indexJoins, _ := p.tryToGetIndexJoin(prop)
			failpoint.Return(indexJoins, true, nil)
		}
	})

	if prop.IsFlashProp() && ((p.preferJoinType&preferBCJoin) == 0 && p.preferJoinType > 0) {
		return nil, false, nil
	}
	if prop.MPPPartitionTp == property.BroadcastType {
		return nil, false, nil
	}
	joins := make([]PhysicalPlan, 0, 8)
	canPushToTiFlash := p.canPushToCop(kv.TiFlash)
	if p.ctx.GetSessionVars().IsMPPAllowed() && canPushToTiFlash {
		if p.shouldUseMPPBCJ() {
			mppJoins := p.tryToGetMppHashJoin(prop, true)
			if (p.preferJoinType & preferBCJoin) > 0 {
				return mppJoins, true, nil
			}
			joins = append(joins, mppJoins...)
		} else {
			mppJoins := p.tryToGetMppHashJoin(prop, false)
			joins = append(joins, mppJoins...)
		}
	} else if p.ctx.GetSessionVars().AllowBCJ && canPushToTiFlash {
		broadCastJoins := p.tryToGetBroadCastJoin(prop)
		if (p.preferJoinType & preferBCJoin) > 0 {
			return broadCastJoins, true, nil
		}
		joins = append(joins, broadCastJoins...)
	}
	if prop.IsFlashProp() {
		return joins, true, nil
	}

	mergeJoins := p.GetMergeJoin(prop, p.schema, p.Stats(), p.children[0].statsInfo(), p.children[1].statsInfo())
	if (p.preferJoinType&preferMergeJoin) > 0 && len(mergeJoins) > 0 {
		return mergeJoins, true, nil
	}
	joins = append(joins, mergeJoins...)

	indexJoins, forced := p.tryToGetIndexJoin(prop)
	if forced {
		return indexJoins, true, nil
	}
	joins = append(joins, indexJoins...)

	hashJoins := p.getHashJoins(prop)
	if (p.preferJoinType&preferHashJoin) > 0 && len(hashJoins) > 0 {
		return hashJoins, true, nil
	}
	joins = append(joins, hashJoins...)

	if p.preferJoinType > 0 {
		// If we reach here, it means we have a hint that doesn't work.
		// It might be affected by the required property, so we enforce
		// this property and try the hint again.
		return joins, false, nil
	}
	return joins, true, nil
}

func canExprsInJoinPushdown(p *LogicalJoin, storeType kv.StoreType) bool {
	equalExprs := make([]expression.Expression, 0, len(p.EqualConditions))
	for _, eqCondition := range p.EqualConditions {
		if eqCondition.FuncName.L == ast.NullEQ {
			return false
		}
		equalExprs = append(equalExprs, eqCondition)
	}
	if !expression.CanExprsPushDown(p.ctx.GetSessionVars().StmtCtx, equalExprs, p.ctx.GetClient(), storeType) {
		return false
	}
	if !expression.CanExprsPushDown(p.ctx.GetSessionVars().StmtCtx, p.LeftConditions, p.ctx.GetClient(), storeType) {
		return false
	}
	if !expression.CanExprsPushDown(p.ctx.GetSessionVars().StmtCtx, p.RightConditions, p.ctx.GetClient(), storeType) {
		return false
	}
	if !expression.CanExprsPushDown(p.ctx.GetSessionVars().StmtCtx, p.OtherConditions, p.ctx.GetClient(), storeType) {
		return false
	}
	return true
}

func (p *LogicalJoin) tryToGetMppHashJoin(prop *property.PhysicalProperty, useBCJ bool) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil
	}

	if p.JoinType != InnerJoin && p.JoinType != LeftOuterJoin && p.JoinType != RightOuterJoin && p.JoinType != SemiJoin && p.JoinType != AntiSemiJoin {
		return nil
	}

	if len(p.EqualConditions) == 0 {
		if p.ctx.GetSessionVars().AllowCartesianBCJ == 0 || !useBCJ {
			return nil
		}
	}
	if (len(p.LeftConditions) != 0 && p.JoinType != LeftOuterJoin) || (len(p.RightConditions) != 0 && p.JoinType != RightOuterJoin) {
		return nil
	}

	if prop.MPPPartitionTp == property.BroadcastType {
		return nil
	}
	if !canExprsInJoinPushdown(p, kv.TiFlash) {
		return nil
	}
	lkeys, rkeys, _, _ := p.GetJoinKeys()
	// check match property
	baseJoin := basePhysicalJoin{
		JoinType:        p.JoinType,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		DefaultValues:   p.DefaultValues,
		LeftJoinKeys:    lkeys,
		RightJoinKeys:   rkeys,
	}
	// It indicates which side is the build side.
	preferredBuildIndex := 0
	if p.JoinType == InnerJoin {
		if p.children[0].statsInfo().Count() > p.children[1].statsInfo().Count() {
			preferredBuildIndex = 1
		}
	} else if p.JoinType == SemiJoin || p.JoinType == AntiSemiJoin {
		preferredBuildIndex = 1
	}
	if p.JoinType == LeftOuterJoin || p.JoinType == RightOuterJoin {
		// TiFlash does not requires that the build side must be the inner table for outer join
		// so we can choose the build side based on the row count, except that
		// 1. it is a broadcast join(for broadcast join, it make sense to use the broadcast side as the build side)
		// 2. or session variable MPPOuterJoinFixedBuildSide is set to true
		// 3. or there are otherConditions for this join
		if useBCJ || p.ctx.GetSessionVars().MPPOuterJoinFixedBuildSide || len(p.OtherConditions) > 0 {
			if p.JoinType == LeftOuterJoin {
				preferredBuildIndex = 1
			}
		} else if p.children[0].statsInfo().Count() > p.children[1].statsInfo().Count() {
			preferredBuildIndex = 1
		}
	}
	baseJoin.InnerChildIdx = preferredBuildIndex
	childrenProps := make([]*property.PhysicalProperty, 2)
	if useBCJ {
		childrenProps[preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.BroadcastType, CanAddEnforcer: true}
		expCnt := math.MaxFloat64
		if prop.ExpectedCnt < p.stats.RowCount {
			expCntScale := prop.ExpectedCnt / p.stats.RowCount
			expCnt = p.children[1-preferredBuildIndex].statsInfo().RowCount * expCntScale
		}
		if prop.MPPPartitionTp == property.HashType {
			hashKeys := rkeys
			if preferredBuildIndex == 1 {
				hashKeys = lkeys
			}
			if matches := prop.IsSubsetOf(hashKeys); len(matches) != 0 {
				childrenProps[1-preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: expCnt, MPPPartitionTp: property.HashType, MPPPartitionCols: prop.MPPPartitionCols}
			} else {
				return nil
			}
		} else {
			childrenProps[1-preferredBuildIndex] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: expCnt, MPPPartitionTp: property.AnyType}
		}
	} else {
		if prop.MPPPartitionTp == property.HashType {
			var matches []int
			if matches = prop.IsSubsetOf(lkeys); len(matches) == 0 {
				matches = prop.IsSubsetOf(rkeys)
			}
			if len(matches) == 0 {
				return nil
			}
			lkeys = chooseSubsetOfJoinKeys(lkeys, matches)
			rkeys = chooseSubsetOfJoinKeys(rkeys, matches)
		}
		childrenProps[0] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.HashType, MPPPartitionCols: lkeys, CanAddEnforcer: true}
		childrenProps[1] = &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.HashType, MPPPartitionCols: rkeys, CanAddEnforcer: true}
	}
	join := PhysicalHashJoin{
		basePhysicalJoin: baseJoin,
		Concurrency:      uint(p.ctx.GetSessionVars().CopTiFlashConcurrencyFactor),
		EqualConditions:  p.EqualConditions,
		storeTp:          kv.TiFlash,
		mppShuffleJoin:   !useBCJ,
		// Mpp Join has quite heavy cost. Even limit might not suspend it in time, so we dont scale the count.
	}.Init(p.ctx, p.stats, p.blockOffset, childrenProps...)
	return []PhysicalPlan{join}
}

func chooseSubsetOfJoinKeys(keys []*expression.Column, matches []int) []*expression.Column {
	newKeys := make([]*expression.Column, 0, len(matches))
	for _, id := range matches {
		newKeys = append(newKeys, keys[id])
	}
	return newKeys
}

func (p *LogicalJoin) tryToGetBroadCastJoin(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && !prop.IsFlashProp() {
		return nil
	}
	if !canExprsInJoinPushdown(p, kv.TiFlash) {
		return nil
	}

	// Disable broadcast join on partition table for TiFlash.
	for _, child := range p.children {
		if ds, isDataSource := child.(*DataSource); isDataSource {
			if ds.tableInfo.GetPartitionInfo() != nil {
				return nil
			}
		}
	}

	if (p.JoinType != InnerJoin && p.JoinType != LeftOuterJoin && p.JoinType != RightOuterJoin && p.JoinType != SemiJoin && p.JoinType != AntiSemiJoin) || len(p.EqualConditions) == 0 {
		return nil
	}

	// for left/semi/anti-semi join the global idx must be 1, and for right join the global idx must be 0
	if hasPrefer, idx := p.getPreferredBCJLocalIndex(); hasPrefer {
		if (idx == 0 && p.JoinType == RightOuterJoin) || (idx == 1 && (p.JoinType == LeftOuterJoin || p.JoinType == SemiJoin || p.JoinType == AntiSemiJoin)) {
			return nil
		}
		return p.tryToGetBroadCastJoinByPreferGlobalIdx(prop, 1-idx)
	}
	if p.JoinType == InnerJoin {
		results := p.tryToGetBroadCastJoinByPreferGlobalIdx(prop, 0)
		results = append(results, p.tryToGetBroadCastJoinByPreferGlobalIdx(prop, 1)...)
		return results
	} else if p.JoinType == LeftOuterJoin || p.JoinType == SemiJoin || p.JoinType == AntiSemiJoin {
		return p.tryToGetBroadCastJoinByPreferGlobalIdx(prop, 1)
	}
	return p.tryToGetBroadCastJoinByPreferGlobalIdx(prop, 0)
}

func (p *LogicalJoin) tryToGetBroadCastJoinByPreferGlobalIdx(prop *property.PhysicalProperty, preferredGlobalIndex int) []PhysicalPlan {
	lkeys, rkeys, _, _ := p.GetJoinKeys()
	baseJoin := basePhysicalJoin{
		JoinType:        p.JoinType,
		LeftConditions:  p.LeftConditions,
		RightConditions: p.RightConditions,
		OtherConditions: p.OtherConditions,
		DefaultValues:   p.DefaultValues,
		LeftJoinKeys:    lkeys,
		RightJoinKeys:   rkeys,
	}

	preferredBuildIndex := 0
	if p.children[0].statsInfo().Count() > p.children[1].statsInfo().Count() {
		preferredBuildIndex = 1
	}
	if p.JoinType == SemiJoin || p.JoinType == AntiSemiJoin {
		preferredBuildIndex = 1
	}
	// TiFlash does not support Right out join with other conditions, if the join
	// has other conditions, need to set the build side to make sure it will be
	// executed as left join in TiFlash(In TiFlash the build side is always the right side)
	if len(p.OtherConditions) != 0 {
		if p.JoinType == RightOuterJoin {
			preferredBuildIndex = 0
		} else if p.JoinType == LeftOuterJoin {
			preferredBuildIndex = 1
		}
	}
	baseJoin.InnerChildIdx = preferredBuildIndex
	childrenReqProps := make([]*property.PhysicalProperty, 2)
	childrenReqProps[preferredGlobalIndex] = &property.PhysicalProperty{TaskTp: property.CopTiFlashGlobalReadTaskType, ExpectedCnt: math.MaxFloat64}
	if prop.TaskTp == property.CopTiFlashGlobalReadTaskType {
		childrenReqProps[1-preferredGlobalIndex] = &property.PhysicalProperty{TaskTp: property.CopTiFlashGlobalReadTaskType, ExpectedCnt: math.MaxFloat64}
	} else {
		childrenReqProps[1-preferredGlobalIndex] = &property.PhysicalProperty{TaskTp: property.CopTiFlashLocalReadTaskType, ExpectedCnt: math.MaxFloat64}
	}
	if prop.ExpectedCnt < p.stats.RowCount {
		expCntScale := prop.ExpectedCnt / p.stats.RowCount
		childrenReqProps[1-baseJoin.InnerChildIdx].ExpectedCnt = p.children[1-baseJoin.InnerChildIdx].statsInfo().RowCount * expCntScale
	}

	join := PhysicalHashJoin{
		basePhysicalJoin: baseJoin,
		Concurrency:      uint(p.ctx.GetSessionVars().CopTiFlashConcurrencyFactor),
		EqualConditions:  p.EqualConditions,
		storeTp:          kv.TiFlash,
		globalChildIndex: preferredGlobalIndex,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, childrenReqProps...)
	return []PhysicalPlan{join}
}

// TryToGetChildProp will check if this sort property can be pushed or not.
// When a sort column will be replaced by scalar function, we refuse it.
// When a sort column will be replaced by a constant, we just remove it.
func (p *LogicalProjection) TryToGetChildProp(prop *property.PhysicalProperty) (*property.PhysicalProperty, bool) {
	newProp := prop.CloneEssentialFields()
	newCols := make([]property.SortItem, 0, len(prop.SortItems))
	for _, col := range prop.SortItems {
		idx := p.schema.ColumnIndex(col.Col)
		switch expr := p.Exprs[idx].(type) {
		case *expression.Column:
			newCols = append(newCols, property.SortItem{Col: expr, Desc: col.Desc})
		case *expression.ScalarFunction:
			return nil, false
		}
	}
	newProp.SortItems = newCols
	return newProp, true
}

func (p *LogicalProjection) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	newProp, ok := p.TryToGetChildProp(prop)
	if !ok {
		return nil, true, nil
	}
	newProps := []*property.PhysicalProperty{newProp}
	// generate a mpp task candidate if enforced mpp
	if newProp.TaskTp != property.MppTaskType && p.SCtx().GetSessionVars().IsMPPEnforced() && p.canPushToCop(kv.TiFlash) &&
		expression.CanExprsPushDown(p.SCtx().GetSessionVars().StmtCtx, p.Exprs, p.SCtx().GetClient(), kv.TiFlash) {
		mppProp := newProp.CloneEssentialFields()
		mppProp.TaskTp = property.MppTaskType
		newProps = append(newProps, mppProp)
	}
	ret := make([]PhysicalPlan, 0, len(newProps))
	for _, newProp := range newProps {
		proj := PhysicalProjection{
			Exprs:                p.Exprs,
			CalculateNoDelay:     p.CalculateNoDelay,
			AvoidColumnEvaluator: p.AvoidColumnEvaluator,
		}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, newProp)
		proj.SetSchema(p.schema)
		ret = append(ret, proj)
	}
	return ret, true, nil
}

func (lt *LogicalTopN) getPhysTopN(prop *property.PhysicalProperty) []PhysicalPlan {
	if lt.limitHints.preferLimitToCop {
		if !lt.canPushToCop(kv.TiKV) {
			errMsg := "Optimizer Hint LIMIT_TO_COP is inapplicable"
			warning := ErrInternal.GenWithStack(errMsg)
			lt.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
			lt.limitHints.preferLimitToCop = false
		}
	}
	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopDoubleReadTaskType}
	if !lt.limitHints.preferLimitToCop {
		allTaskTypes = append(allTaskTypes, property.RootTaskType)
	}
	if lt.ctx.GetSessionVars().IsMPPAllowed() {
		allTaskTypes = append(allTaskTypes, property.MppTaskType)
	}
	ret := make([]PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: math.MaxFloat64}
		topN := PhysicalTopN{
			ByItems: lt.ByItems,
			Count:   lt.Count,
			Offset:  lt.Offset,
		}.Init(lt.ctx, lt.stats, lt.blockOffset, resultProp)
		ret = append(ret, topN)
	}
	return ret
}

func (lt *LogicalTopN) getPhysLimits(prop *property.PhysicalProperty) []PhysicalPlan {
	p, canPass := GetPropByOrderByItems(lt.ByItems)
	if !canPass {
		return nil
	}

	if lt.limitHints.preferLimitToCop {
		if !lt.canPushToCop(kv.TiKV) {
			errMsg := "Optimizer Hint LIMIT_TO_COP is inapplicable"
			warning := ErrInternal.GenWithStack(errMsg)
			lt.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
			lt.limitHints.preferLimitToCop = false
		}
	}

	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopDoubleReadTaskType}
	if !lt.limitHints.preferLimitToCop {
		allTaskTypes = append(allTaskTypes, property.RootTaskType)
	}
	ret := make([]PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(lt.Count + lt.Offset), SortItems: p.SortItems}
		limit := PhysicalLimit{
			Count:  lt.Count,
			Offset: lt.Offset,
		}.Init(lt.ctx, lt.stats, lt.blockOffset, resultProp)
		limit.SetSchema(lt.Schema())
		ret = append(ret, limit)
	}
	return ret
}

// MatchItems checks if this prop's columns can match by items totally.
func MatchItems(p *property.PhysicalProperty, items []*util.ByItems) bool {
	if len(items) < len(p.SortItems) {
		return false
	}
	for i, col := range p.SortItems {
		sortItem := items[i]
		if sortItem.Desc != col.Desc || !sortItem.Expr.Equal(nil, col.Col) {
			return false
		}
	}
	return true
}

func (lt *LogicalTopN) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if MatchItems(prop, lt.ByItems) {
		return append(lt.getPhysTopN(prop), lt.getPhysLimits(prop)...), true, nil
	}
	return nil, true, nil
}

// GetHashJoin is public for cascades planner.
func (la *LogicalApply) GetHashJoin(prop *property.PhysicalProperty) *PhysicalHashJoin {
	return la.LogicalJoin.getHashJoin(prop, 1, false)
}

func (la *LogicalApply) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if !prop.AllColsFromSchema(la.children[0].Schema()) || prop.IsFlashProp() { // for convenient, we don't pass through any prop
		la.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `Apply` is not supported now.")
		return nil, true, nil
	}
	disableAggPushDownToCop(la.children[0])
	join := la.GetHashJoin(prop)
	var columns = make([]*expression.Column, 0, len(la.CorCols))
	for _, colColumn := range la.CorCols {
		columns = append(columns, &colColumn.Column)
	}
	cacheHitRatio := 0.0
	if la.stats.RowCount != 0 {
		ndv := getCardinality(columns, la.schema, la.stats)
		// for example, if there are 100 rows and the number of distinct values of these correlated columns
		// are 70, then we can assume 30 rows can hit the cache so the cache hit ratio is 1 - (70/100) = 0.3
		cacheHitRatio = 1 - (ndv / la.stats.RowCount)
	}

	var canUseCache bool
	if cacheHitRatio > 0.1 && la.ctx.GetSessionVars().MemQuotaApplyCache > 0 {
		canUseCache = true
	} else {
		canUseCache = false
	}

	apply := PhysicalApply{
		PhysicalHashJoin: *join,
		OuterSchema:      la.CorCols,
		CanUseCache:      canUseCache,
	}.Init(la.ctx,
		la.stats.ScaleByExpectCnt(prop.ExpectedCnt),
		la.blockOffset,
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, SortItems: prop.SortItems},
		&property.PhysicalProperty{ExpectedCnt: math.MaxFloat64})
	apply.SetSchema(la.schema)
	return []PhysicalPlan{apply}, true, nil
}

func disableAggPushDownToCop(p LogicalPlan) {
	for _, child := range p.Children() {
		disableAggPushDownToCop(child)
	}
	if agg, ok := p.(*LogicalAggregation); ok {
		agg.noCopPushDown = true
	}
}

func (p *LogicalWindow) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
		"MPP mode may be blocked because operator `Window` is not supported now.")
	if prop.IsFlashProp() {
		return nil, true, nil
	}
	var byItems []property.SortItem
	byItems = append(byItems, p.PartitionBy...)
	byItems = append(byItems, p.OrderBy...)
	childProperty := &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, SortItems: byItems, CanAddEnforcer: true}
	if !prop.IsPrefix(childProperty) {
		return nil, true, nil
	}
	window := PhysicalWindow{
		WindowFuncDescs: p.WindowFuncDescs,
		PartitionBy:     p.PartitionBy,
		OrderBy:         p.OrderBy,
		Frame:           p.Frame,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, childProperty)
	window.SetSchema(p.Schema())
	return []PhysicalPlan{window}, true, nil
}

// exhaustPhysicalPlans is only for implementing interface. DataSource and Dual generate task in `findBestTask` directly.
func (p *baseLogicalPlan) exhaustPhysicalPlans(_ *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	panic("baseLogicalPlan.exhaustPhysicalPlans() should never be called.")
}

// canPushToCop checks if it can be pushed to some stores. For TiKV, it only checks datasource.
// For TiFlash, it will check whether the operator is supported, but note that the check might be inaccrute.
func (p *baseLogicalPlan) canPushToCop(storeTp kv.StoreType) bool {
	return p.canPushToCopImpl(storeTp, false)
}

func (p *baseLogicalPlan) canPushToCopImpl(storeTp kv.StoreType, considerDual bool) bool {
	ret := true
	for _, ch := range p.children {
		switch c := ch.(type) {
		case *DataSource:
			validDs := false
			for _, path := range c.possibleAccessPaths {
				if path.StoreType == storeTp {
					validDs = true
				}
			}
			ret = ret && validDs
		case *LogicalUnionAll:
			if storeTp == kv.TiFlash {
				ret = ret && c.canPushToCopImpl(storeTp, true)
			} else {
				return false
			}
		case *LogicalProjection:
			if storeTp == kv.TiFlash {
				ret = ret && c.canPushToCopImpl(storeTp, considerDual)
			} else {
				return false
			}
		case *LogicalTableDual:
			return storeTp == kv.TiFlash && considerDual
		case *LogicalAggregation, *LogicalSelection, *LogicalJoin:
			if storeTp == kv.TiFlash {
				ret = ret && c.canPushToCop(storeTp)
			} else {
				return false
			}
		// These operators can be partially push down to TiFlash, so we don't raise warning for them.
		case *LogicalLimit, *LogicalTopN:
			return false
		default:
			p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
				"MPP mode may be blocked because operator `" + c.TP() + "` is not supported now.")
			return false
		}
	}
	return ret
}

func (la *LogicalAggregation) canPushToCop(storeTp kv.StoreType) bool {
	return la.baseLogicalPlan.canPushToCop(storeTp) && !la.noCopPushDown
}

func (la *LogicalAggregation) getEnforcedStreamAggs(prop *property.PhysicalProperty) []PhysicalPlan {
	if prop.IsFlashProp() {
		return nil
	}
	_, desc := prop.AllSameOrder()
	allTaskTypes := prop.GetAllPossibleChildTaskTypes()
	enforcedAggs := make([]PhysicalPlan, 0, len(allTaskTypes))
	childProp := &property.PhysicalProperty{
		ExpectedCnt:    math.Max(prop.ExpectedCnt*la.inputCount/la.stats.RowCount, prop.ExpectedCnt),
		CanAddEnforcer: true,
		SortItems:      property.SortItemsFromCols(la.GetGroupByCols(), desc),
	}
	if !prop.IsPrefix(childProp) {
		return enforcedAggs
	}
	taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopDoubleReadTaskType}
	if la.HasDistinct() {
		// TODO: remove AllowDistinctAggPushDown after the cost estimation of distinct pushdown is implemented.
		// If AllowDistinctAggPushDown is set to true, we should not consider RootTask.
		if !la.canPushToCop(kv.TiKV) || !la.ctx.GetSessionVars().AllowDistinctAggPushDown {
			taskTypes = []property.TaskType{property.RootTaskType}
		}
	} else if !la.aggHints.preferAggToCop {
		taskTypes = append(taskTypes, property.RootTaskType)
	}
	for _, taskTp := range taskTypes {
		copiedChildProperty := new(property.PhysicalProperty)
		*copiedChildProperty = *childProp // It's ok to not deep copy the "cols" field.
		copiedChildProperty.TaskTp = taskTp

		agg := basePhysicalAgg{
			GroupByItems: la.GroupByItems,
			AggFuncs:     la.AggFuncs,
		}.initForStream(la.ctx, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), la.blockOffset, copiedChildProperty)
		agg.SetSchema(la.schema.Clone())
		enforcedAggs = append(enforcedAggs, agg)
	}
	return enforcedAggs
}

func (la *LogicalAggregation) distinctArgsMeetsProperty() bool {
	for _, aggFunc := range la.AggFuncs {
		if aggFunc.HasDistinct {
			for _, distinctArg := range aggFunc.Args {
				if !expression.Contains(la.GroupByItems, distinctArg) {
					return false
				}
			}
		}
	}
	return true
}

func (la *LogicalAggregation) getStreamAggs(prop *property.PhysicalProperty) []PhysicalPlan {
	// TODO: support CopTiFlash task type in stream agg
	if prop.IsFlashProp() {
		return nil
	}
	all, desc := prop.AllSameOrder()
	if !all {
		return nil
	}

	for _, aggFunc := range la.AggFuncs {
		if aggFunc.Mode == aggregation.FinalMode {
			return nil
		}
	}
	// group by a + b is not interested in any order.
	groupByCols := la.GetGroupByCols()
	if len(groupByCols) != len(la.GroupByItems) {
		return nil
	}

	allTaskTypes := prop.GetAllPossibleChildTaskTypes()
	streamAggs := make([]PhysicalPlan, 0, len(la.possibleProperties)*(len(allTaskTypes)-1)+len(allTaskTypes))
	childProp := &property.PhysicalProperty{
		ExpectedCnt: math.Max(prop.ExpectedCnt*la.inputCount/la.stats.RowCount, prop.ExpectedCnt),
	}

	for _, possibleChildProperty := range la.possibleProperties {
		childProp.SortItems = property.SortItemsFromCols(possibleChildProperty[:len(groupByCols)], desc)
		if !prop.IsPrefix(childProp) {
			continue
		}
		// The table read of "CopDoubleReadTaskType" can't promises the sort
		// property that the stream aggregation required, no need to consider.
		taskTypes := []property.TaskType{property.CopSingleReadTaskType}
		if la.HasDistinct() {
			// TODO: remove AllowDistinctAggPushDown after the cost estimation of distinct pushdown is implemented.
			// If AllowDistinctAggPushDown is set to true, we should not consider RootTask.
			if !la.ctx.GetSessionVars().AllowDistinctAggPushDown {
				taskTypes = []property.TaskType{property.RootTaskType}
			} else if !la.distinctArgsMeetsProperty() {
				continue
			}
		} else if !la.aggHints.preferAggToCop {
			taskTypes = append(taskTypes, property.RootTaskType)
		}
		if !la.canPushToCop(kv.TiKV) {
			taskTypes = []property.TaskType{property.RootTaskType}
		}
		for _, taskTp := range taskTypes {
			copiedChildProperty := new(property.PhysicalProperty)
			*copiedChildProperty = *childProp // It's ok to not deep copy the "cols" field.
			copiedChildProperty.TaskTp = taskTp

			agg := basePhysicalAgg{
				GroupByItems: la.GroupByItems,
				AggFuncs:     la.AggFuncs,
			}.initForStream(la.ctx, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), la.blockOffset, copiedChildProperty)
			agg.SetSchema(la.schema.Clone())
			streamAggs = append(streamAggs, agg)
		}
	}
	// If STREAM_AGG hint is existed, it should consider enforce stream aggregation,
	// because we can't trust possibleChildProperty completely.
	if (la.aggHints.preferAggType & preferStreamAgg) > 0 {
		streamAggs = append(streamAggs, la.getEnforcedStreamAggs(prop)...)
	}
	return streamAggs
}

// TODO: support more operators and distinct later
func (la *LogicalAggregation) checkCanPushDownToMPP() bool {
	hasUnsupportedDistinct := false
	for _, agg := range la.AggFuncs {
		// MPP does not support distinct except count distinct now
		if agg.HasDistinct {
			if agg.Name != ast.AggFuncCount {
				hasUnsupportedDistinct = true
			}
		}
		// MPP does not support AggFuncApproxCountDistinct now
		if agg.Name == ast.AggFuncApproxCountDistinct {
			hasUnsupportedDistinct = true
		}
	}
	if hasUnsupportedDistinct {
		if la.ctx.GetSessionVars().StmtCtx.InExplainStmt {
			la.ctx.GetSessionVars().StmtCtx.AppendWarning(errors.New("Aggregation can not be pushed to storage layer in mpp mode because it contains agg function with distinct"))
		}
		return false
	}
	return CheckAggCanPushCop(la.ctx, la.AggFuncs, la.GroupByItems, kv.TiFlash)
}

func (la *LogicalAggregation) tryToGetMppHashAggs(prop *property.PhysicalProperty) (hashAggs []PhysicalPlan) {
	if !prop.IsEmpty() {
		return nil
	}
	if prop.TaskTp != property.RootTaskType && prop.TaskTp != property.MppTaskType {
		return nil
	}
	if prop.MPPPartitionTp == property.BroadcastType {
		return nil
	}
	if len(la.GroupByItems) > 0 {
		partitionCols := la.GetGroupByCols()
		// trying to match the required parititions.
		if prop.MPPPartitionTp == property.HashType {
			if matches := prop.IsSubsetOf(partitionCols); len(matches) != 0 {
				partitionCols = chooseSubsetOfJoinKeys(partitionCols, matches)
			} else {
				// do not satisfy the property of its parent, so return empty
				return nil
			}
		}
		// TODO: permute various partition columns from group-by columns
		// 1-phase agg
		// If there are no available partition cols, but still have group by items, that means group by items are all expressions or constants.
		// To avoid mess, we don't do any one-phase aggregation in this case.
		if len(partitionCols) != 0 {
			childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.HashType, MPPPartitionCols: partitionCols, CanAddEnforcer: true}
			agg := NewPhysicalHashAgg(la, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), childProp)
			agg.SetSchema(la.schema.Clone())
			agg.MppRunMode = Mpp1Phase
			hashAggs = append(hashAggs, agg)
		}

		// 2-phase agg
		childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64, MPPPartitionTp: property.AnyType}
		agg := NewPhysicalHashAgg(la, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), childProp)
		agg.SetSchema(la.schema.Clone())
		agg.MppRunMode = Mpp2Phase
		agg.MppPartitionCols = partitionCols
		hashAggs = append(hashAggs, agg)

		// agg runs on TiDB with a partial agg on TiFlash if possible
		if prop.TaskTp == property.RootTaskType {
			childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64}
			agg := NewPhysicalHashAgg(la, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), childProp)
			agg.SetSchema(la.schema.Clone())
			agg.MppRunMode = MppTiDB
			hashAggs = append(hashAggs, agg)
		}
	} else {
		// TODO: support scalar agg in MPP, merge the final result to one node
		childProp := &property.PhysicalProperty{TaskTp: property.MppTaskType, ExpectedCnt: math.MaxFloat64}
		agg := NewPhysicalHashAgg(la, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), childProp)
		agg.SetSchema(la.schema.Clone())
		if la.HasDistinct() {
			agg.MppRunMode = MppScalar
		} else {
			agg.MppRunMode = MppTiDB
		}
		hashAggs = append(hashAggs, agg)
	}
	return
}

func (la *LogicalAggregation) getHashAggs(prop *property.PhysicalProperty) []PhysicalPlan {
	if !prop.IsEmpty() {
		return nil
	}
	if prop.TaskTp == property.MppTaskType && !la.checkCanPushDownToMPP() {
		return nil
	}
	hashAggs := make([]PhysicalPlan, 0, len(prop.GetAllPossibleChildTaskTypes()))
	taskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopDoubleReadTaskType}
	if la.ctx.GetSessionVars().AllowBCJ {
		taskTypes = append(taskTypes, property.CopTiFlashLocalReadTaskType)
	}
	canPushDownToTiFlash := la.canPushToCop(kv.TiFlash)
	canPushDownToMPP := canPushDownToTiFlash && la.ctx.GetSessionVars().IsMPPAllowed() && la.checkCanPushDownToMPP()
	if la.HasDistinct() {
		// TODO: remove after the cost estimation of distinct pushdown is implemented.
		if !la.ctx.GetSessionVars().AllowDistinctAggPushDown {
			if la.ctx.GetSessionVars().StmtCtx.InExplainStmt {
				la.ctx.GetSessionVars().StmtCtx.AppendWarning(errors.New("Aggregation can not be pushed to storage layer in non-mpp mode because it contains agg function with distinct"))
			}
			taskTypes = []property.TaskType{property.RootTaskType}
		}
	} else if !la.aggHints.preferAggToCop {
		taskTypes = append(taskTypes, property.RootTaskType)
	}
	if !la.canPushToCop(kv.TiKV) {
		taskTypes = []property.TaskType{property.RootTaskType}
		if canPushDownToTiFlash {
			taskTypes = append(taskTypes, property.CopTiFlashLocalReadTaskType)
		}
	}
	if canPushDownToMPP {
		taskTypes = append(taskTypes, property.MppTaskType)
	}
	if prop.IsFlashProp() {
		taskTypes = []property.TaskType{prop.TaskTp}
	}
	for _, taskTp := range taskTypes {
		if taskTp == property.MppTaskType {
			mppAggs := la.tryToGetMppHashAggs(prop)
			if len(mppAggs) > 0 {
				hashAggs = append(hashAggs, mppAggs...)
			}
		} else {
			agg := NewPhysicalHashAgg(la, la.stats.ScaleByExpectCnt(prop.ExpectedCnt), &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64, TaskTp: taskTp})
			agg.SetSchema(la.schema.Clone())
			hashAggs = append(hashAggs, agg)
		}
	}
	return hashAggs
}

// ResetHintIfConflicted resets the aggHints.preferAggType if they are conflicted,
// and returns the two preferAggType hints.
func (la *LogicalAggregation) ResetHintIfConflicted() (preferHash bool, preferStream bool) {
	preferHash = (la.aggHints.preferAggType & preferHashAgg) > 0
	preferStream = (la.aggHints.preferAggType & preferStreamAgg) > 0
	if preferHash && preferStream {
		errMsg := "Optimizer aggregation hints are conflicted"
		warning := ErrInternal.GenWithStack(errMsg)
		la.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
		la.aggHints.preferAggType = 0
		preferHash, preferStream = false, false
	}
	return
}

func (la *LogicalAggregation) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if la.aggHints.preferAggToCop {
		if !la.canPushToCop(kv.TiKV) {
			errMsg := "Optimizer Hint AGG_TO_COP is inapplicable"
			warning := ErrInternal.GenWithStack(errMsg)
			la.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
			la.aggHints.preferAggToCop = false
		}
	}

	preferHash, preferStream := la.ResetHintIfConflicted()

	hashAggs := la.getHashAggs(prop)
	if hashAggs != nil && preferHash {
		return hashAggs, true, nil
	}

	streamAggs := la.getStreamAggs(prop)
	if streamAggs != nil && preferStream {
		return streamAggs, true, nil
	}

	aggs := append(hashAggs, streamAggs...)

	if streamAggs == nil && preferStream && !prop.IsEmpty() {
		errMsg := "Optimizer Hint STREAM_AGG is inapplicable"
		warning := ErrInternal.GenWithStack(errMsg)
		la.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
	}

	return aggs, !(preferStream || preferHash), nil
}

func (p *LogicalSelection) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	childProp := prop.CloneEssentialFields()
	sel := PhysicalSelection{
		Conditions: p.Conditions,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, childProp)
	return []PhysicalPlan{sel}, true, nil
}

func (p *LogicalLimit) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if !prop.IsEmpty() {
		return nil, true, nil
	}

	if p.limitHints.preferLimitToCop {
		if !p.canPushToCop(kv.TiKV) {
			errMsg := "Optimizer Hint LIMIT_TO_COP is inapplicable"
			warning := ErrInternal.GenWithStack(errMsg)
			p.ctx.GetSessionVars().StmtCtx.AppendWarning(warning)
			p.limitHints.preferLimitToCop = false
		}
	}

	allTaskTypes := []property.TaskType{property.CopSingleReadTaskType, property.CopDoubleReadTaskType}
	if !p.limitHints.preferLimitToCop {
		allTaskTypes = append(allTaskTypes, property.RootTaskType)
	}
	if p.canPushToCop(kv.TiFlash) && p.ctx.GetSessionVars().IsMPPAllowed() {
		allTaskTypes = append(allTaskTypes, property.MppTaskType)
	}
	ret := make([]PhysicalPlan, 0, len(allTaskTypes))
	for _, tp := range allTaskTypes {
		resultProp := &property.PhysicalProperty{TaskTp: tp, ExpectedCnt: float64(p.Count + p.Offset)}
		limit := PhysicalLimit{
			Offset: p.Offset,
			Count:  p.Count,
		}.Init(p.ctx, p.stats, p.blockOffset, resultProp)
		limit.SetSchema(p.Schema())
		ret = append(ret, limit)
	}
	return ret, true, nil
}

func (p *LogicalLock) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced(
			"MPP mode may be blocked because operator `Lock` is not supported now.")
		return nil, true, nil
	}
	childProp := prop.CloneEssentialFields()
	lock := PhysicalLock{
		Lock:             p.Lock,
		TblID2Handle:     p.tblID2Handle,
		PartitionedTable: p.partitionedTable,
		ExtraPIDInfo:     p.extraPIDInfo,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), childProp)
	return []PhysicalPlan{lock}, true, nil
}

func (p *LogicalUnionAll) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	// TODO: UnionAll can not pass any order, but we can change it to sort merge to keep order.
	if !prop.IsEmpty() || (prop.IsFlashProp() && prop.TaskTp != property.MppTaskType) {
		return nil, true, nil
	}
	// TODO: UnionAll can pass partition info, but for briefness, we prevent it from pushing down.
	if prop.TaskTp == property.MppTaskType && prop.MPPPartitionTp != property.AnyType {
		return nil, true, nil
	}
	canUseMpp := p.ctx.GetSessionVars().IsMPPAllowed() && p.canPushToCopImpl(kv.TiFlash, true)
	chReqProps := make([]*property.PhysicalProperty, 0, len(p.children))
	for range p.children {
		if canUseMpp && prop.TaskTp == property.MppTaskType {
			chReqProps = append(chReqProps, &property.PhysicalProperty{
				ExpectedCnt: prop.ExpectedCnt,
				TaskTp:      property.MppTaskType,
			})
		} else {
			chReqProps = append(chReqProps, &property.PhysicalProperty{ExpectedCnt: prop.ExpectedCnt})
		}
	}
	ua := PhysicalUnionAll{
		mpp: canUseMpp && prop.TaskTp == property.MppTaskType,
	}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, chReqProps...)
	ua.SetSchema(p.Schema())
	if canUseMpp && prop.TaskTp == property.RootTaskType {
		chReqProps = make([]*property.PhysicalProperty, 0, len(p.children))
		for range p.children {
			chReqProps = append(chReqProps, &property.PhysicalProperty{
				ExpectedCnt: prop.ExpectedCnt,
				TaskTp:      property.MppTaskType,
			})
		}
		mppUA := PhysicalUnionAll{mpp: true}.Init(p.ctx, p.stats.ScaleByExpectCnt(prop.ExpectedCnt), p.blockOffset, chReqProps...)
		mppUA.SetSchema(p.Schema())
		return []PhysicalPlan{ua, mppUA}, true, nil
	}
	return []PhysicalPlan{ua}, true, nil
}

func (p *LogicalPartitionUnionAll) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	uas, flagHint, err := p.LogicalUnionAll.exhaustPhysicalPlans(prop)
	if err != nil {
		return nil, false, err
	}
	for _, ua := range uas {
		ua.(*PhysicalUnionAll).tp = plancodec.TypePartitionUnion
	}
	return uas, flagHint, nil
}

func (ls *LogicalSort) getPhysicalSort(prop *property.PhysicalProperty) *PhysicalSort {
	ps := PhysicalSort{ByItems: ls.ByItems}.Init(ls.ctx, ls.stats.ScaleByExpectCnt(prop.ExpectedCnt), ls.blockOffset, &property.PhysicalProperty{ExpectedCnt: math.MaxFloat64})
	return ps
}

func (ls *LogicalSort) getNominalSort(reqProp *property.PhysicalProperty) *NominalSort {
	prop, canPass, onlyColumn := GetPropByOrderByItemsContainScalarFunc(ls.ByItems)
	if !canPass {
		return nil
	}
	prop.ExpectedCnt = reqProp.ExpectedCnt
	ps := NominalSort{OnlyColumn: onlyColumn, ByItems: ls.ByItems}.Init(
		ls.ctx, ls.stats.ScaleByExpectCnt(prop.ExpectedCnt), ls.blockOffset, prop)
	return ps
}

func (ls *LogicalSort) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if MatchItems(prop, ls.ByItems) {
		ret := make([]PhysicalPlan, 0, 2)
		ret = append(ret, ls.getPhysicalSort(prop))
		ns := ls.getNominalSort(prop)
		if ns != nil {
			ret = append(ret, ns)
		}
		return ret, true, nil
	}
	return nil, true, nil
}

func (p *LogicalMaxOneRow) exhaustPhysicalPlans(prop *property.PhysicalProperty) ([]PhysicalPlan, bool, error) {
	if !prop.IsEmpty() || prop.IsFlashProp() {
		p.SCtx().GetSessionVars().RaiseWarningWhenMPPEnforced("MPP mode may be blocked because operator `MaxOneRow` is not supported now.")
		return nil, true, nil
	}
	mor := PhysicalMaxOneRow{}.Init(p.ctx, p.stats, p.blockOffset, &property.PhysicalProperty{ExpectedCnt: 2})
	return []PhysicalPlan{mor}, true, nil
}
