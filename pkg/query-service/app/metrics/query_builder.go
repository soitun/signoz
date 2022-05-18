package metrics

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/Knetic/govaluate"
	"go.signoz.io/query-service/constants"
	"go.signoz.io/query-service/model"
)

type CHMetricQueries struct {
	Queries        []string
	FormulaQueries []string
	Err            error
}

var AggregateOperatorToPercentile = map[model.AggregateOperator]float64{
	model.P05: 0.5,
	model.P10: 0.10,
	model.P20: 0.20,
	model.P25: 0.25,
	model.P50: 0.50,
	model.P75: 0.75,
	model.P90: 0.90,
	model.P95: 0.95,
	model.P99: 0.99,
}

var AggregateOperatorToSQLFunc = map[model.AggregateOperator]string{
	model.AVG:      "avg",
	model.MAX:      "max",
	model.MIN:      "min",
	model.SUM:      "sum",
	model.RATE_SUM: "sum",
	model.RATE_AVG: "avg",
	model.RATE_MAX: "max",
	model.RATE_MIN: "min",
}

var SupportedFunctions = []string{"exp", "log", "ln", "exp2", "log2", "exp10", "log10", "sqrt", "cbrt", "erf", "erfc", "lgamma", "tgamma", "sin", "cos", "tan", "asin", "acos", "atan", "degrees", "radians"}

func GoValuateFuncs() map[string]govaluate.ExpressionFunction {
	var GoValuateFuncs = map[string]govaluate.ExpressionFunction{}
	for _, fn := range SupportedFunctions {
		GoValuateFuncs[fn] = func(args ...interface{}) (interface{}, error) {
			return nil, nil
		}
	}
	return GoValuateFuncs
}

// formattedValue formats the value to be used in clickhouse query
func formattedValue(v interface{}) string {
	switch x := v.(type) {
	case int:
		return fmt.Sprintf("%d", x)
	case float32, float64:
		return fmt.Sprintf("%f", x)
	case string:
		return fmt.Sprintf("'%s'", x)
	case bool:
		return fmt.Sprintf("%v", x)
	case []interface{}:
		switch x[0].(type) {
		case string:
			str := "["
			for idx, sVal := range x {
				str += fmt.Sprintf("'%s'", sVal)
				if idx != len(x)-1 {
					str += ","
				}
			}
			str += "]"
			return str
		case int, float32, float64, bool:
			return strings.Join(strings.Fields(fmt.Sprint(x)), ",")
		}
		return ""
	default:
		// may be log the warning here?
		return ""
	}
}

// BuildMetricsTimeSeriesFilterQuery builds the sub-query to be used for filtering
// timeseries based on search criteria
func BuildMetricsTimeSeriesFilterQuery(fs *model.FilterSet, tableName string) (string, error) {
	queryString := ""
	for idx, item := range fs.Items {
		fmtVal := formattedValue(item.Value)
		switch op := strings.ToLower(item.Operation); op {
		case "eq":
			queryString += fmt.Sprintf("JSONExtractString(%s.labels,'%s') = %s", tableName, item.Key, fmtVal)
		case "neq":
			queryString += fmt.Sprintf("JSONExtractString(%s.labels,'%s') != %s", tableName, item.Key, fmtVal)
		case "in":
			queryString += fmt.Sprintf("JSONExtractString(%s.labels,'%s') IN %s", tableName, item.Key, fmtVal)
		case "nin":
			queryString += fmt.Sprintf("JSONExtractString(%s.labels,'%s') NOT IN %s", tableName, item.Key, fmtVal)
		case "like":
			queryString += fmt.Sprintf("like(JSONExtractString(%s.labels,'%s'), %s)", tableName, item.Key, fmtVal)
		case "match":
			queryString += fmt.Sprintf("match(JSONExtractString(%s.labels,'%s'), %s)", tableName, item.Key, fmtVal)
		default:
			return "", fmt.Errorf("unsupported operation")
		}
		if idx != len(fs.Items)-1 {
			queryString += " " + fs.Operation + " "
		}
	}

	filterSubQuery := fmt.Sprintf("SELECT fingerprint, labels FROM %s.%s WHERE %s", constants.SIGNOZ_METRIC_DBNAME, constants.SIGNOZ_TIMESERIES_TABLENAME, queryString)

	return filterSubQuery, nil
}

func BuildMetricQuery(qp *model.QueryRangeParamsV2, mq *model.MetricQuery, tableName string) (string, error) {
	nameFilterItem := model.FilterItem{Key: "__name__", Value: mq.MetricName, Operation: "EQ"}
	if mq.TagFilters == nil {
		mq.TagFilters = &model.FilterSet{Operation: "AND", Items: []model.FilterItem{
			nameFilterItem,
		}}
	} else {
		mq.TagFilters.Items = append(mq.TagFilters.Items, nameFilterItem)
	}

	filterSubQuery, err := BuildMetricsTimeSeriesFilterQuery(mq.TagFilters, tableName)
	if err != nil {
		return "", err
	}

	samplesTableTimeFilter := fmt.Sprintf("timestamp_ms >= %d AND timestamp_ms < %d", qp.Start, qp.End)

	// Select the aggregate value for interval
	intermediateResult :=
		"SELECT fingerprint, %s" +
			" toStartOfInterval(toDateTime(intDiv(timestamp_ms, 1000)), INTERVAL %d SECOND) as ts, " +
			" %s as res" +
			" FROM " + constants.SIGNOZ_METRIC_DBNAME + "." + constants.SIGNOZ_SAMPLES_TABLENAME +
			" INNER JOIN " +
			" (%s) as filtered_time_series " +
			" USING fingerprint " +
			" WHERE " + samplesTableTimeFilter +
			" GROUP BY %s " +
			" ORDER BY fingerprint, ts"

	groupBy := groupBy(mq.GroupingTags)
	groupTags := groupSelect(mq.GroupingTags)

	switch mq.AggregateOperator {
	case model.RATE_SUM, model.RATE_MAX, model.RATE_AVG, model.RATE_MIN:
		op := fmt.Sprintf("%s(value)", AggregateOperatorToSQLFunc[mq.AggregateOperator])
		sub_query := fmt.Sprintf(intermediateResult, groupTags, qp.Step, op, filterSubQuery, groupBy)
		query := `SELECT %s ts, runningDifference(res)/runningDifference(ts) as res FROM(%s)`
		query = fmt.Sprintf(query, groupTags, sub_query)
		return query, nil
	case model.P05, model.P10, model.P20, model.P25, model.P50, model.P75, model.P90, model.P95, model.P99:
		op := fmt.Sprintf("quantile(%v)(value)", AggregateOperatorToPercentile[mq.AggregateOperator])
		query := fmt.Sprintf(intermediateResult, groupTags, qp.Step, op, filterSubQuery, groupBy)
		return query, nil
	case model.AVG, model.SUM, model.MIN, model.MAX:
		op := fmt.Sprintf("%s(value)", AggregateOperatorToSQLFunc[mq.AggregateOperator])
		query := fmt.Sprintf(intermediateResult, groupTags, qp.Step, op, filterSubQuery, groupBy)
		return query, nil
	case model.COUNT:
		op := "count(*)"
		query := fmt.Sprintf(intermediateResult, groupTags, qp.Step, op, filterSubQuery, groupBy)
		return query, nil
	case model.COUNT_DISTINCT:
	default:
		return "", fmt.Errorf("unsupported aggregate operator")
	}
	return "", fmt.Errorf("unsupported aggregate operator")
}

func groupBy(tags []string) string {
	groupByFilter := "fingerprint, ts"
	for _, tag := range tags {
		groupByFilter += fmt.Sprintf(", JSONExtractString(labels,'%s') as %s", tag, tag)
	}
	return groupByFilter
}

func groupSelect(tags []string) string {
	groupTags := strings.Join(tags, ",")
	if len(tags) != 0 {
		groupTags += ","
	}
	return groupTags
}

// BuildQueries builds the queries to be executed for query_range timeseries API
func BuildQueries(qp *model.QueryRangeParamsV2, tableName string) *CHMetricQueries {

	if qp.CompositeMetricQuery.RawQuery != "" {
		return &CHMetricQueries{Queries: []string{qp.CompositeMetricQuery.RawQuery}}
	}

	var queries []string
	var formulaQueries []string

	varToQuery := make(map[string]string)
	for _, formula := range qp.CompositeMetricQuery.Formulas {
		expression, err := govaluate.NewEvaluableExpressionWithFunctions(formula, GoValuateFuncs())
		if err != nil {
			return &CHMetricQueries{Err: fmt.Errorf("invalid expression")}
		}

		for _, var_ := range expression.Vars() {
			if _, ok := varToQuery[var_]; !ok {
				mq := qp.CompositeMetricQuery.BuildMetricQueries[var_]
				query, err := BuildMetricQuery(qp, mq, tableName)
				if err != nil {
					return &CHMetricQueries{Err: err}
				}
				varToQuery[var_] = query
			}
		}
	}

	for _, formula := range qp.CompositeMetricQuery.Formulas {
		expression, _ := govaluate.NewEvaluableExpressionWithFunctions(formula, GoValuateFuncs())
		tokens := expression.Tokens()
		if len(tokens) == 1 {
			var_ := tokens[0].Value.(string)
			queries = append(queries, varToQuery[var_])
			continue
		}
		vars := expression.Vars()
		for idx, var_ := range vars[1:] {
			x, y := vars[idx], var_
			if !reflect.DeepEqual(qp.CompositeMetricQuery.BuildMetricQueries[x].GroupingTags, qp.CompositeMetricQuery.BuildMetricQueries[y].GroupingTags) {
				return &CHMetricQueries{Err: fmt.Errorf("group by must be same")}
			}
		}
		var modified []govaluate.ExpressionToken
		for idx := range tokens {
			token := tokens[idx]
			if token.Kind == govaluate.VARIABLE {
				val, _ := token.Value.(string)
				token.Value = val + ".res"
			}
			modified = append(modified, token)
		}
		govaluate.NewEvaluableExpressionFromTokens(modified)

		var formulaSubQuery string
		for idx, var_ := range vars {
			query := varToQuery[var_]
			groupTags := groupSelect(qp.CompositeMetricQuery.BuildMetricQueries[var_].GroupingTags)
			formulaSubQuery += fmt.Sprintf("(%s) as %s ", query, var_)
			if idx < len(vars)-1 {
				formulaSubQuery += "INNER JOIN"
			} else if len(vars) > 1 {
				formulaSubQuery += fmt.Sprintf("USING (ts %s)", groupTags)
			}
		}
		formulaQuery := fmt.Sprintf("SELECT ts, %s as res FROM ", formula) + formulaSubQuery
		formulaQueries = append(formulaQueries, formulaQuery)
	}
	fmt.Println(queries, formulaQueries)
	return &CHMetricQueries{queries, []string{}, nil}
}