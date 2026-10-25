package review

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type relationshipPair struct {
	path   string
	left   diff.Piece
	right  diff.Piece
	reason string
}

type relationshipFunction struct {
	name       string
	start, end int
	references map[string]struct{}
	shared     map[string]struct{}
}

type relationshipPromptContext struct {
	required string
	optional string
}

// PrepareRelationshipChunks reserves required scope before fitting optional context.
func PrepareRelationshipChunks(model Model, chunks []diff.Chunk, files []diff.FileContext, minimumImportance int, required, optional string, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	support := relationshipPromptContext{required: required, optional: optional}
	settings := policy.RelationshipReview()
	if !settings.Enabled {
		return chunks, nil
	}
	var err error
	chunks, err = subdivideRelationshipSources(model, chunks, files, minimumImportance, support, policy, maximumBytes)
	if err != nil {
		return nil, err
	}
	pairs := sourceRelationshipPairs(chunks, files, settings.MaximumPairs+1)
	result := slices.Clone(chunks)
	for index, pair := range pairs {
		if index >= settings.MaximumPairs {
			gap, err := relationshipGap(pair.path, "relationship_pair_limit", len(pairs)-index, policy)
			if err != nil {
				return nil, err
			}
			result = append(result, gap)
			break
		}
		joint, err := prepareRelationshipPair(model, pair, minimumImportance, support, policy, maximumBytes)
		if err != nil {
			return nil, err
		}
		result = append(result, joint)
	}
	for index := range result {
		result[index].Index, result[index].Total = index+1, len(result)
	}
	// Final indices can change the rendered size across decimal boundaries.
	for index := len(chunks); index < len(result); index++ {
		if !result[index].CoverageComplete {
			continue
		}
		size, err := relationshipRequestBytes(model, result[index], minimumImportance, support, policy, maximumBytes)
		if err != nil {
			return nil, err
		}
		if size > maximumBytes {
			gap, err := relationshipGap(result[index].Paths[0], "relationship_request_limit", 1, policy)
			if err != nil {
				return nil, err
			}
			gap.Index, gap.Total = index+1, len(result)
			result[index] = gap
		}
	}
	return result, nil
}

func subdivideRelationshipSources(model Model, chunks []diff.Chunk, files []diff.FileContext, minimumImportance int, support relationshipPromptContext, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	paths := make(map[string]bool)
	pairs := sourceRelationshipPairs(chunks, files, policy.RelationshipReview().MaximumPairs+1)
	if len(pairs) > policy.RelationshipReview().MaximumPairs {
		return chunks, nil
	}
	for _, pair := range pairs {
		paths[pair.path] = true
	}
	var result []diff.Chunk
	for _, chunk := range chunks {
		var parts []diff.Chunk
		changed := false
		for _, piece := range chunk.Pieces {
			part := relationshipSourceChunk(piece)
			divided := []diff.Chunk{part}
			if paths[piece.Path] && piece.Metadata == nil && piece.CoverageComplete && !piece.Oversized {
				var err error
				divided, err = divideRelationshipSource(model, part, minimumImportance, support, policy, maximumBytes)
				if err != nil {
					return nil, err
				}
			}
			changed = changed || len(divided) > 1
			parts = append(parts, divided...)
		}
		if changed {
			result = append(result, parts...)
		} else {
			result = append(result, chunk)
		}
	}
	return result, nil
}

func divideRelationshipSource(model Model, chunk diff.Chunk, minimumImportance int, support relationshipPromptContext, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	piece := chunk.Pieces[0]
	pair := relationshipPair{path: piece.Path, left: piece, right: piece, reason: "source_budget"}
	joint, err := renderRelationshipPair(pair, true, policy)
	if err != nil {
		return nil, err
	}
	size, err := relationshipRequestBytes(model, joint, minimumImportance, support, policy, maximumBytes)
	if err != nil {
		return nil, err
	}
	if size <= maximumBytes {
		return []diff.Chunk{chunk}, nil
	}
	left, right, divided := chunk.Split()
	if !divided {
		return []diff.Chunk{chunk}, nil
	}
	first, err := divideRelationshipSource(model, left, minimumImportance, support, policy, maximumBytes)
	if err != nil {
		return nil, err
	}
	second, err := divideRelationshipSource(model, right, minimumImportance, support, policy, maximumBytes)
	if err != nil {
		return nil, err
	}
	return append(first, second...), nil
}

func relationshipSourceChunk(piece diff.Piece) diff.Chunk {
	var metadata []diff.MetadataSource
	if piece.Metadata != nil {
		metadata = []diff.MetadataSource{*piece.Metadata}
	}
	return diff.Chunk{RelationshipReview: false, Index: 1, Total: 1, Text: piece.Text, Pieces: []diff.Piece{piece}, Paths: []string{piece.Path}, Metadata: metadata, CoverageComplete: piece.CoverageComplete}
}

func prepareRelationshipPair(model Model, pair relationshipPair, minimumImportance int, support relationshipPromptContext, policy reviewrules.Policy, maximumBytes int) (diff.Chunk, error) {
	for _, compact := range []bool{false, true} {
		joint, err := renderRelationshipPair(pair, compact, policy)
		if err != nil {
			return diff.Chunk{}, err
		}
		size, err := relationshipRequestBytes(model, joint, minimumImportance, support, policy, maximumBytes)
		if err != nil {
			return diff.Chunk{}, err
		}
		if size <= maximumBytes {
			return joint, nil
		}
	}
	return relationshipGap(pair.path, "relationship_request_limit", 1, policy)
}

func relationshipRequestBytes(model Model, chunk diff.Chunk, minimumImportance int, support relationshipPromptContext, policy reviewrules.Policy, maximumBytes int) (int, error) {
	prompt, err := BuildModelReviewPrompt(model, chunk, minimumImportance, support.required, support.optional, policy, maximumBytes)
	if err != nil {
		var exceeded *diff.PromptSizeError
		if errors.As(err, &exceeded) {
			return exceeded.Actual, nil
		}
		return 0, err
	}
	if sizer, ok := model.(RequestSizer); ok {
		size, err := sizer.ReviewRequestBytes(prompt)
		if err != nil {
			slog.Warn("Relationship provider request sizing failed", "err", err)
			return 0, fmt.Errorf("measure relationship provider request: %w", err)
		}
		return size, nil
	}
	return len(prompt), nil
}

func renderRelationshipPair(pair relationshipPair, compact bool, policy reviewrules.Policy) (diff.Chunk, error) {
	text := make([]string, 0, 2)
	for _, piece := range []diff.Piece{pair.left, pair.right} {
		content := piece.Text
		if compact {
			var err error
			var data reviewrules.PromptData
			data.Path, data.Input = pair.path, piece.Patch
			content, err = policy.Render("relationship.fragment", data)
			if err != nil {
				slog.Warn("Relationship fragment rendering failed", "err", err)
				return diff.Chunk{}, fmt.Errorf("render relationship fragment: %w", err)
			}
		}
		text = append(text, content)
	}
	body := strings.Join(text, "\n\n")
	// One piece with several complete hunks keeps both endpoints together.
	// A later budget reduction must fail instead of splitting this assessment.
	patch := strings.TrimRight(pair.left.Patch, "\n") + "\n" + pair.right.Patch
	piece := diff.Piece{Path: pair.path, Text: body, Patch: patch, Header: pair.reason + ": " + pair.left.Header + "; " + pair.right.Header, Metadata: nil, CoverageComplete: true, Oversized: false}
	return diff.Chunk{RelationshipReview: true, Index: 1, Total: 1, Text: body, Pieces: []diff.Piece{piece}, Paths: []string{pair.path}, Metadata: nil, CoverageComplete: true}, nil
}

func relationshipGap(path, reason string, remaining int, policy reviewrules.Policy) (diff.Chunk, error) {
	var data reviewrules.PromptData
	data.Path, data.Relationship, data.Total = path, reason, remaining
	body, err := policy.Render("relationship.gap", data)
	if err != nil {
		slog.Warn("Relationship gap rendering failed", "err", err)
		return diff.Chunk{}, fmt.Errorf("render relationship gap: %w", err)
	}
	piece := diff.Piece{Path: path, Text: body, Patch: "", Header: reason, Metadata: nil, CoverageComplete: false, Oversized: true}
	return diff.Chunk{RelationshipReview: true, Index: 1, Total: 1, Text: body, Pieces: []diff.Piece{piece}, Paths: []string{path}, Metadata: nil, CoverageComplete: false}, nil
}

func sourceRelationshipPairs(chunks []diff.Chunk, files []diff.FileContext, limit int) []relationshipPair {
	byPath := make(map[string][]diff.Piece)
	groups := make(map[string][]int)
	for group, chunk := range chunks {
		for _, piece := range chunk.Pieces {
			if piece.Metadata == nil && piece.CoverageComplete && !piece.Oversized && piece.Patch != "" {
				byPath[piece.Path] = append(byPath[piece.Path], piece)
				groups[piece.Path] = append(groups[piece.Path], group)
			}
		}
	}
	var result []relationshipPair
	for _, file := range files {
		pieces := byPath[file.Path]
		if len(pieces) < 2 {
			continue
		}
		links := make(map[[2]int]string)
		for index := 1; index < len(pieces); index++ {
			links[[2]int{index - 1, index}] = "adjacent_fragments"
		}
		if filepath.Ext(file.Path) == ".go" && len(links) < limit {
			linkGoRelationships(file, pieces, links, limit)
		}
		ordered := make([][2]int, 0, len(links))
		for pair := range links {
			if groups[file.Path][pair[0]] != groups[file.Path][pair[1]] {
				ordered = append(ordered, pair)
			}
		}
		slices.SortFunc(ordered, func(left, right [2]int) int {
			if left[0] != right[0] {
				return left[0] - right[0]
			}
			return left[1] - right[1]
		})
		for _, pair := range ordered {
			result = append(result, relationshipPair{path: file.Path, left: pieces[pair[0]], right: pieces[pair[1]], reason: links[pair]})
			if len(result) >= limit {
				return result
			}
		}
	}
	return result
}

func linkGoRelationships(file diff.FileContext, pieces []diff.Piece, links map[[2]int]string, limit int) {
	positions := token.NewFileSet()
	source, err := parser.ParseFile(positions, file.Path, file.CurrentContent, 0)
	if err != nil {
		slog.Debug("Go relationship analysis uses adjacent fragments after a parse failure", "path", file.Path, "err", err)
		return
	}
	functions := scanGoRelationshipFunctions(source, positions)
	groups := make(map[int][]int)
	for pieceIndex, piece := range pieces {
		for _, functionIndex := range fragmentFunctionIndices(piece, functions) {
			groups[functionIndex] = append(groups[functionIndex], pieceIndex)
		}
	}
	for first := range functions {
		for second := first + 1; second < len(functions); second++ {
			left, right := groups[first], groups[second]
			if len(left) == 0 || len(right) == 0 || !functionsRelated(functions[first], functions[second]) {
				continue
			}
			pair := [2]int{left[len(left)-1], right[0]}
			if pair[0] != pair[1] {
				links[pair] = "go_function_reference"
			}
			if len(links) >= limit {
				return
			}
		}
	}
}

func scanGoRelationshipFunctions(source *ast.File, positions *token.FileSet) []relationshipFunction {
	var functions []relationshipFunction
	globals := relationshipGlobals(source)
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		references := make(map[string]struct{})
		shared := make(map[string]struct{})
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if identifier, ok := node.(*ast.Ident); ok {
				references[identifier.Name] = struct{}{}
				if _, global := globals[identifier.Name]; global {
					shared[identifier.Name] = struct{}{}
				}
			}
			return true
		})
		if function.Recv != nil {
			for _, receiver := range function.Recv.List {
				ast.Inspect(receiver.Type, func(node ast.Node) bool {
					if identifier, ok := node.(*ast.Ident); ok {
						shared["receiver:"+identifier.Name] = struct{}{}
					}
					return true
				})
			}
		}
		functions = append(functions, relationshipFunction{name: function.Name.Name, start: positions.Position(function.Pos()).Line, end: positions.Position(function.End()).Line, references: references, shared: shared})
	}
	return functions
}

func relationshipGlobals(source *ast.File) map[string]struct{} {
	globals := make(map[string]struct{})
	for _, declaration := range source.Decls {
		if general, ok := declaration.(*ast.GenDecl); ok {
			for _, specification := range general.Specs {
				if values, ok := specification.(*ast.ValueSpec); ok {
					for _, name := range values.Names {
						globals[name.Name] = struct{}{}
					}
				}
			}
		}
	}
	return globals
}

func fragmentFunctionIndices(piece diff.Piece, functions []relationshipFunction) []int {
	var chunk diff.Chunk
	chunk.Pieces = []diff.Piece{piece}
	rows, err := diff.ChangedSourceRows(chunk)
	if err != nil {
		return nil
	}
	var result []int
	for index, function := range functions {
		for _, row := range rows {
			if row.RightLine >= function.start && row.RightLine <= function.end {
				result = append(result, index)
				break
			}
		}
	}
	return result
}

func functionsRelated(first, second relationshipFunction) bool {
	_, forward := first.references[second.name]
	_, backward := second.references[first.name]
	if forward || backward {
		return true
	}
	for reference := range first.shared {
		if _, shared := second.shared[reference]; shared {
			return true
		}
	}
	return false
}
