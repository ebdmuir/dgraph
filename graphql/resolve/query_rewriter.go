/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package resolve

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/dgraph-io/dgraph/x"

	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/graphql/schema"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/pkg/errors"
)

type queryRewriter struct{}

// NewQueryRewriter returns a new QueryRewriter.
func NewQueryRewriter() QueryRewriter {
	return &queryRewriter{}
}

// Rewrite rewrites a GraphQL query into a Dgraph GraphQuery.
func (qr *queryRewriter) Rewrite(ctx context.Context,
	gqlQuery schema.Query) (*gql.GraphQuery, error) {

	authVariables, err := x.ExtractAuthVariables(ctx)
	if err != nil {
		return nil, err
	}

	switch gqlQuery.QueryType() {
	case schema.GetQuery:

		// TODO: The only error that can occur in query rewriting is if an ID argument
		// can't be parsed as a uid: e.g. the query was something like:
		//
		// getT(id: "HI") { ... }
		//
		// But that's not a rewriting error!  It should be caught by validation
		// way up when the query first comes in.  All other possible problems with
		// the query are caught by validation.
		// ATM, I'm not sure how to hook into the GraphQL validator to get that to happen
		xid, uid, err := gqlQuery.IDArgValue()
		if err != nil {
			return nil, err
		}

		dgQuery := rewriteAsGet(gqlQuery, uid, xid)
		addTypeFilter(dgQuery, gqlQuery.Type())

		return addAuth(dgQuery, gqlQuery, authVariables), nil

	case schema.FilterQuery:
		dgQuery := rewriteAsQuery(gqlQuery)
		return addAuth(dgQuery, gqlQuery, authVariables), nil
	case schema.PasswordQuery:
		return passwordQuery(gqlQuery)
	default:
		return nil, errors.Errorf("unimplemented query type %s", gqlQuery.QueryType())
	}
}

type Authorizer struct {
	authVariables map[string]string
	rbacRule      map[int]bool
}

func (a *Authorizer) evaluateRBACRule(rule *schema.RuleAst) bool {
	value := rule.GetOperand()
	if value.GetName() == "eq" && value.GetOperand().GetName() == a.authVariables[rule.GetName()] {
		return true
	}
	return false
}

func (a *Authorizer) adjustQuery(query *gql.GraphQuery, field schema.Field) {
	sch := field.Operation().Schema()

	find := func(f schema.Field) (*gql.GraphQuery, int) {
		for index, i := range query.Children {
			if (f.Alias() != "" && f.Alias() == i.Alias) || f.Name() == i.Alias {
				return i, index
			}
		}
		return nil, 0
	}

	for _, f := range field.SelectionSet() {
		child, ind := find(f)
		if child == nil {
			continue
		}

		if len(f.SelectionSet()) > 0 {
			a.adjustQuery(child, f)
			authRules := sch.AuthTypeRules(f.Type().Name())
			if authRules != nil && authRules.Query != nil {
				filters := authRules.Query.GetFilter(a.rbacRule)
				if child.Filter != nil {
					child.Filter = &gql.FilterTree{
						Op: "and",
						Child: []*gql.FilterTree{
							filters,
							child.Filter,
						},
					}
				} else {
					child.Filter = filters
				}
			}

		} else {
			authRules := sch.AuthFieldRules(f.GetObjectName(), f.Name())
			if authRules != nil && authRules.Query != nil {
				copy(query.Children[ind:], query.Children[ind+1:])
				query.Children[len(query.Children)-1] = nil
				query.Children = query.Children[:len(query.Children)-1]
			}
		}
	}
}

func (a *Authorizer) getRBACRules(node *schema.RuleNode) {
	a.rbacRule[node.RuleID] = true
	valid := true
	for _, rule := range node.Or {
		if rule.IsRBAC() {
			a.getRBACRules(rule)
			if a.rbacRule[rule.RuleID] {
				valid = false
			}
		}
		a.rbacRule[node.RuleID] = valid
	}

	for _, rule := range node.And {
		if rule.IsRBAC() {
			a.getRBACRules(rule)
			if !a.rbacRule[rule.RuleID] {
				valid = false
			}
		}
		a.rbacRule[node.RuleID] = valid
	}

	if node.Not != nil && node.Not.IsRBAC() {
		a.getRBACRules(node.Not)
		a.rbacRule[node.Not.RuleID] = !a.rbacRule[node.Not.RuleID]
		a.rbacRule[node.RuleID] = a.rbacRule[node.Not.RuleID]
	}

	if node.Rule != nil && node.Rule.IsJWT() {
		a.rbacRule[node.RuleID] = a.evaluateRBACRule(node.Rule)
	}
}

func addAuth(dgQuery *gql.GraphQuery, field schema.Query,
	authVariables map[string]string) *gql.GraphQuery {
	queriedType := field.Type()
	authRules := field.Operation().Schema().AuthTypeRules(queriedType.Name())
	rbacRule := make(map[int]bool)
	var query gql.GraphQuery
	a := Authorizer{authVariables: authVariables, rbacRule: rbacRule}
	query.Children = append(query.Children, dgQuery)
	query.Children = append(query.Children, a.getAuthQueries(field)...)
	a.adjustQuery(dgQuery, field)

	if authRules == nil || authRules.Query == nil {
		return &query
	}

	a.getRBACRules(authRules.Query)
	authFilter := authRules.Query.GetFilter(rbacRule)
	if dgQuery.Filter != nil && authFilter != nil {
		dgQuery.Filter = &gql.FilterTree{
			Op: "and",
			Child: []*gql.FilterTree{
				authFilter,
				dgQuery.Filter,
			},
		}
	} else if authFilter != nil {
		dgQuery.Filter = authFilter
	}

	return &query
}

func (a *Authorizer) getAuthQueries(field schema.Field) []*gql.GraphQuery {
	visited := make(map[string]bool)
	queue := []*schema.Field{&field}
	result := []*gql.GraphQuery{}

	sch := field.Operation().Schema()

	for ind := 0; ind < len(queue); ind++ {
		i := queue[ind]
		typeName := (*i).Type().Name()
		visited[typeName] = true
		authRules := sch.AuthTypeRules(typeName)

		if authRules != nil && authRules.Query != nil {
			a.getRBACRules(authRules.Query)
			if val, ok := a.rbacRule[authRules.Query.RuleID]; !ok || val {
				result = append(result, authRules.Query.GetQueries(a.authVariables)...)
			}
		}

		for _, f := range (*i).SelectionSet() {
			if f.Skip() || !f.Include() || f.Name() == schema.Typename {
				continue
			}

			if len(f.SelectionSet()) > 0 && !visited[f.Type().Name()] {
				queue = append(queue, &f)
			}
		}

	}

	fieldQueries := a.generateFieldQueries(field, []*schema.Field{&field})
	seen := make(map[string]bool)

	j := 0
	for _, i := range fieldQueries {
		if seen[i.Attr] {
			continue
		}
		seen[i.Attr] = true
		fieldQueries[j] = i
		j++
	}

	fieldQueries = fieldQueries[:j]
	result = append(result, fieldQueries...)
	return result
}

func (a *Authorizer) generateFieldQueries(field schema.Field, path []*schema.Field) []*gql.GraphQuery {
	sch := field.Operation().Schema()
	var result []*gql.GraphQuery

	for _, f := range field.SelectionSet() {
		if f.Skip() || !f.Include() || f.Name() == schema.Typename {
			continue
		}

		path = append(path, &f)

		if len(f.SelectionSet()) > 0 {
			result = append(result, a.generateFieldQueries(f, path)...)
		} else {
			authRules := sch.AuthFieldRules(f.GetObjectName(), f.Name())
			filterPut := false
			if authRules != nil && authRules.Query != nil {
				a.getRBACRules(authRules.Query)
				if val, ok := a.rbacRule[authRules.Query.RuleID]; ok && !val {
					continue
				}
				var query *gql.GraphQuery
				name := ""

				for i := len(path) - 1; i >= 1; i-- {
					name = (*path[i]).Name() + name
					child := &gql.GraphQuery{}
					f := *path[i]
					if f.Alias() != "" {
						child.Alias = f.Alias()
					} else {
						child.Alias = f.Name()
					}

					if f.Type().Name() == schema.IDType {
						child.Attr = "uid"
					} else {
						child.Attr = f.DgraphPredicate()
					}

					if query != nil {
						child.Children = []*gql.GraphQuery{{
							Attr: "uid",
						}, query}

						if !filterPut {
							child.Filter = authRules.Query.GetFilter(a.rbacRule)
							filterPut = true
						}
					}

					query = child
				}

				last := &gql.GraphQuery{
					Attr: fmt.Sprintf("%s.%s", (*path[0]).ResponseName(), name),
					Func: &gql.Function{
						Name: "type",
						Args: []gql.Arg{{Value: (*path[0]).Type().Name()}},
					},
					Children: []*gql.GraphQuery{{
						Attr: "uid",
					}, query},
					Cascade: true,
				}

				if !filterPut {
					last.Filter = authRules.Query.GetFilter(a.rbacRule)
				}

				result = append(result, last)
				result = append(result, authRules.Query.GetQueries(a.authVariables)...)
			}
		}

		path = path[:len(path)-1]
	}

	return result
}

func passwordQuery(m schema.Query) (*gql.GraphQuery, error) {
	xid, uid, err := m.IDArgValue()
	if err != nil {
		return nil, err
	}

	dgQuery := rewriteAsGet(m, uid, xid)

	queriedType := m.Type()
	name := queriedType.PasswordField().Name()
	predicate := queriedType.DgraphPredicate(name)
	password := m.ArgValue(name).(string)

	op := &gql.GraphQuery{
		Attr:   "checkPwd",
		Func:   dgQuery.Func,
		Filter: dgQuery.Filter,
		Children: []*gql.GraphQuery{{
			Var: "pwd",
			Attr: fmt.Sprintf(`checkpwd(%s, "%s")`, predicate,
				password),
		}},
	}

	ft := &gql.FilterTree{
		Op: "and",
		Child: []*gql.FilterTree{{
			Func: &gql.Function{
				Name: "eq",
				Args: []gql.Arg{
					{
						Value: "val(pwd)",
					},
					{
						Value: "1",
					},
				},
			},
		}},
	}

	if dgQuery.Filter != nil {
		ft.Child = append(ft.Child, dgQuery.Filter)
	}

	dgQuery.Filter = ft

	qry := &gql.GraphQuery{
		Children: []*gql.GraphQuery{dgQuery, op},
	}

	return qry, nil
}

func intersection(a, b []uint64) []uint64 {
	m := make(map[uint64]bool)
	var c []uint64

	for _, item := range a {
		m[item] = true
	}

	for _, item := range b {
		if _, ok := m[item]; ok {
			c = append(c, item)
		}
	}

	return c
}

// addUID adds UID for every node that we query. Otherwise we can't tell the
// difference in a query result between a node that's missing and a node that's
// missing a single value.  E.g. if we are asking for an Author and only the
// 'text' of all their posts e.g. getAuthor(id: 0x123) { posts { text } }
// If the author has 10 posts but three of them have a title, but no text,
// then Dgraph would just return 7 posts.  And we'd have no way of knowing if
// there's only 7 posts, or if there's more that are missing 'text'.
// But, for GraphQL, we want to know about those missing values.
func addUID(dgQuery *gql.GraphQuery) {
	if len(dgQuery.Children) == 0 {
		return
	}
	for _, c := range dgQuery.Children {
		addUID(c)
	}

	uidChild := &gql.GraphQuery{
		Attr:  "uid",
		Alias: "dgraph.uid",
	}
	dgQuery.Children = append(dgQuery.Children, uidChild)
}

func rewriteAsQueryByIds(field schema.Field, uids []uint64) *gql.GraphQuery {
	dgQuery := &gql.GraphQuery{
		Attr: field.ResponseName(),
		Func: &gql.Function{
			Name: "uid",
			UID:  uids,
		},
	}

	if ids := idFilter(field, field.Type().IDField()); ids != nil {
		addUIDFunc(dgQuery, intersection(ids, uids))
	}

	addArgumentsToField(dgQuery, field)
	return dgQuery
}

// addArgumentsToField adds various different arguments to a field, such as
// filter, order, pagination and selection set.
func addArgumentsToField(dgQuery *gql.GraphQuery, field schema.Field) {
	filter, _ := field.ArgValue("filter").(map[string]interface{})
	addFilter(dgQuery, field.Type(), filter)
	addOrder(dgQuery, field)
	addPagination(dgQuery, field)
	addSelectionSetFrom(dgQuery, field)
	addUID(dgQuery)
}

func rewriteAsGet(field schema.Field, uid uint64, xid *string) *gql.GraphQuery {
	if xid == nil {
		return rewriteAsQueryByIds(field, []uint64{uid})
	}

	xidArgName := field.XIDArg()
	eqXidFunc := &gql.Function{
		Name: "eq",
		Args: []gql.Arg{
			{Value: xidArgName},
			{Value: maybeQuoteArg("eq", *xid)},
		},
	}

	var dgQuery *gql.GraphQuery
	if uid > 0 {
		dgQuery = &gql.GraphQuery{
			Attr: field.ResponseName(),
			Func: &gql.Function{
				Name: "uid",
				UID:  []uint64{uid},
			},
		}
		dgQuery.Filter = &gql.FilterTree{
			Func: eqXidFunc,
		}

	} else {
		dgQuery = &gql.GraphQuery{
			Attr: field.ResponseName(),
			Func: eqXidFunc,
		}
	}
	addSelectionSetFrom(dgQuery, field)
	addUID(dgQuery)
	return dgQuery
}

func rewriteAsQuery(field schema.Field) *gql.GraphQuery {
	dgQuery := &gql.GraphQuery{
		Attr: field.ResponseName(),
	}

	if ids := idFilter(field, field.Type().IDField()); ids != nil {
		addUIDFunc(dgQuery, ids)
	} else {
		addTypeFunc(dgQuery, field.Type().DgraphName())
	}

	addArgumentsToField(dgQuery, field)

	return dgQuery
}

func addTypeFilter(q *gql.GraphQuery, typ schema.Type) {
	thisFilter := &gql.FilterTree{
		Func: &gql.Function{
			Name: "type",
			Args: []gql.Arg{{Value: typ.DgraphName()}},
		},
	}

	if q.Filter == nil {
		q.Filter = thisFilter
	} else {
		q.Filter = &gql.FilterTree{
			Op:    "and",
			Child: []*gql.FilterTree{q.Filter, thisFilter},
		}
	}
}

func addUIDFunc(q *gql.GraphQuery, uids []uint64) {
	q.Func = &gql.Function{
		Name: "uid",
		UID:  uids,
	}
}

func addTypeFunc(q *gql.GraphQuery, typ string) {
	q.Func = &gql.Function{
		Name: "type",
		Args: []gql.Arg{{Value: typ}},
	}

}

func addSelectionSetFrom(q *gql.GraphQuery, field schema.Field) {
	// Only add dgraph.type as a child if this field is an interface type and has some children.
	// dgraph.type would later be used in completeObject as different objects in the resulting
	// JSON would return different fields based on their concrete type.
	if field.InterfaceType() && len(field.SelectionSet()) > 0 {
		q.Children = append(q.Children, &gql.GraphQuery{
			Attr: "dgraph.type",
		})
	}

	for _, f := range field.SelectionSet() {
		// We skip typename because we can generate the information from schema or
		// dgraph.type depending upon if the type is interface or not. For interface type
		// we always query dgraph.type and can pick up the value from there.
		if f.Skip() || !f.Include() || f.Name() == schema.Typename {
			continue
		}

		child := &gql.GraphQuery{}

		if f.Alias() != "" {
			child.Alias = f.Alias()
		} else {
			child.Alias = f.Name()
		}

		if f.Type().Name() == schema.IDType {
			child.Attr = "uid"
		} else {
			child.Attr = f.DgraphPredicate()
		}

		filter, _ := f.ArgValue("filter").(map[string]interface{})
		addFilter(child, f.Type(), filter)
		addOrder(child, f)
		addPagination(child, f)

		addSelectionSetFrom(child, f)

		q.Children = append(q.Children, child)
	}
}

func addOrder(q *gql.GraphQuery, field schema.Field) {
	orderArg := field.ArgValue("order")
	order, ok := orderArg.(map[string]interface{})
	for ok {
		ascArg := order["asc"]
		descArg := order["desc"]
		thenArg := order["then"]

		if asc, ok := ascArg.(string); ok {
			q.Order = append(q.Order,
				&pb.Order{Attr: field.Type().DgraphPredicate(asc)})
		} else if desc, ok := descArg.(string); ok {
			q.Order = append(q.Order,
				&pb.Order{Attr: field.Type().DgraphPredicate(desc), Desc: true})
		}

		order, ok = thenArg.(map[string]interface{})
	}
}

func addPagination(q *gql.GraphQuery, field schema.Field) {
	q.Args = make(map[string]string)

	first := field.ArgValue("first")
	if first != nil {
		q.Args["first"] = fmt.Sprintf("%v", first)
	}

	offset := field.ArgValue("offset")
	if offset != nil {
		q.Args["offset"] = fmt.Sprintf("%v", offset)
	}
}

func convertIDs(idsSlice []interface{}) []uint64 {
	ids := make([]uint64, 0, len(idsSlice))
	for _, id := range idsSlice {
		uid, err := strconv.ParseUint(id.(string), 0, 64)
		if err != nil {
			// Skip sending the is part of the query to Dgraph.
			continue
		}
		ids = append(ids, uid)
	}
	return ids
}

func idFilter(field schema.Field, idField schema.FieldDefinition) []uint64 {
	filter, ok := field.ArgValue("filter").(map[string]interface{})
	if !ok || idField == nil {
		return nil
	}

	idsFilter := filter[idField.Name()]
	if idsFilter == nil {
		return nil
	}
	idsSlice := idsFilter.([]interface{})
	return convertIDs(idsSlice)
}

func addFilter(q *gql.GraphQuery, typ schema.Type, filter map[string]interface{}) {
	if len(filter) == 0 {
		return
	}

	// There are two cases here.
	// 1. It could be the case of a filter at root.  In this case we would have added a uid
	// function at root. Lets delete the ids key so that it isn't added in the filter.
	// Also, we need to add a dgraph.type filter.
	// 2. This could be a deep filter. In that case we don't need to do anything special.
	idField := typ.IDField()
	idName := ""
	if idField != nil {
		idName = idField.Name()
	}

	_, hasIDsFilter := filter[idName]
	filterAtRoot := hasIDsFilter && q.Func != nil && q.Func.Name == "uid"
	if filterAtRoot {
		// If id was present as a filter,
		delete(filter, idName)
	}
	q.Filter = buildFilter(typ, filter)
	if filterAtRoot {
		addTypeFilter(q, typ)
	}
}

// buildFilter builds a Dgraph gql.FilterTree from a GraphQL 'filter' arg.
//
// All the 'filter' args built by the GraphQL layer look like
// filter: { title: { anyofterms: "GraphQL" }, ... }
// or
// filter: { title: { anyofterms: "GraphQL" }, isPublished: true, ... }
// or
// filter: { title: { anyofterms: "GraphQL" }, and: { not: { ... } } }
// etc
//
// typ is the GraphQL type we are filtering on, and is needed to turn for example
// title (the GraphQL field) into Post.title (to Dgraph predicate).
//
// buildFilter turns any one filter object into a conjunction
// eg:
// filter: { title: { anyofterms: "GraphQL" }, isPublished: true }
// into:
// @filter(anyofterms(Post.title, "GraphQL") AND eq(Post.isPublished, true))
//
// Filters with `or:` and `not:` get translated to Dgraph OR and NOT.
//
// TODO: There's cases that don't make much sense like
// filter: { or: { title: { anyofterms: "GraphQL" } } }
// ATM those will probably generate junk that might cause a Dgraph error.  And
// bubble back to the user as a GraphQL error when the query fails. Really,
// they should fail query validation and never get here.
func buildFilter(typ schema.Type, filter map[string]interface{}) *gql.FilterTree {

	var ands []*gql.FilterTree
	var or *gql.FilterTree

	// Get a stable ordering so we generate the same thing each time.
	var keys []string
	for key := range filter {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Each key in filter is either "and", "or", "not" or the field name it
	// applies to such as "title" in: `title: { anyofterms: "GraphQL" }``
	for _, field := range keys {
		switch field {

		// In 'and', 'or' and 'not' cases, filter[field] must be a map[string]interface{}
		// or it would have failed GraphQL validation - e.g. 'filter: { and: 10 }'
		// would have failed validation.

		case "and":
			// title: { anyofterms: "GraphQL" }, and: { ... }
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") AND ... )
			ft := buildFilter(typ, filter[field].(map[string]interface{}))
			ands = append(ands, ft)
		case "or":
			// title: { anyofterms: "GraphQL" }, or: { ... }
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") OR ... )
			or = buildFilter(typ, filter[field].(map[string]interface{}))
		case "not":
			// title: { anyofterms: "GraphQL" }, not: { isPublished: true}
			//                       we are here ^^
			// ->
			// @filter(anyofterms(Post.title, "GraphQL") AND NOT eq(Post.isPublished, true))
			not := buildFilter(typ, filter[field].(map[string]interface{}))
			ands = append(ands,
				&gql.FilterTree{
					Op:    "not",
					Child: []*gql.FilterTree{not},
				})
		default:
			// It's a base case like:
			// title: { anyofterms: "GraphQL" } ->  anyofterms(Post.title: "GraphQL")

			switch dgFunc := filter[field].(type) {
			case map[string]interface{}:
				// title: { anyofterms: "GraphQL" } ->  anyofterms(Post.title, "GraphQL")
				// OR
				// numLikes: { le: 10 } -> le(Post.numLikes, 10)
				fn, val := first(dgFunc)
				ands = append(ands, &gql.FilterTree{
					Func: &gql.Function{
						Name: fn,
						Args: []gql.Arg{
							{Value: typ.DgraphPredicate(field)},
							{Value: maybeQuoteArg(fn, val)},
						},
					},
				})
			case []interface{}:
				// ids: [ 0x123, 0x124 ] -> uid(0x123, 0x124)
				ids := convertIDs(dgFunc)
				ands = append(ands, &gql.FilterTree{
					Func: &gql.Function{
						Name: "uid",
						UID:  ids,
					},
				})
			case interface{}:
				// isPublished: true -> eq(Post.isPublished, true)
				// OR an enum case
				// postType: Question -> eq(Post.postType, "Question")
				fn := "eq"
				ands = append(ands, &gql.FilterTree{
					Func: &gql.Function{
						Name: fn,
						Args: []gql.Arg{
							{Value: typ.DgraphPredicate(field)},
							{Value: fmt.Sprintf("%v", dgFunc)},
						},
					},
				})
			}
		}
	}

	var andFt *gql.FilterTree
	if len(ands) == 1 {
		andFt = ands[0]
	} else if len(ands) > 1 {
		andFt = &gql.FilterTree{
			Op:    "and",
			Child: ands,
		}
	}

	if or == nil {
		return andFt
	}

	return &gql.FilterTree{
		Op:    "or",
		Child: []*gql.FilterTree{andFt, or},
	}
}

func maybeQuoteArg(fn string, arg interface{}) string {
	switch arg := arg.(type) {
	case string: // dateTime also parsed as string
		if fn == "regexp" {
			return arg
		}
		return fmt.Sprintf("%q", arg)
	default:
		return fmt.Sprintf("%v", arg)
	}
}

// fst returns the first element it finds in a map - we bump into lots of one-element
// maps like { "anyofterms": "GraphQL" }.  fst helps extract that single mapping.
func first(aMap map[string]interface{}) (string, interface{}) {
	for key, val := range aMap {
		return key, val
	}
	return "", nil
}
