package internal

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"github.com/8treenet/gcache/option"
	"reflect"
	"regexp"
	"strings"
	"unsafe"

	"github.com/jinzhu/gorm"
)

func newEasyScope(s *gorm.Scope, h *Handle) *easyScope {
	es := new(easyScope)
	es.Scope = s.New(s.Value)
	es.forgeSearch = (*search)(unsafe.Pointer(s.Search))
	es.opt = &option.ModelOption{}
	es.opt.Opt = h.cp.defaultOpt.Opt
	es.sourceScope = s
	es.optionSetting()
	es.Table = es.TableName()
	es.primaryFieldName = es.PrimaryField().Name
	es.handle = h

	if models, ok := s.Get(whereModelsSearch); ok {
		list, ok := models.([]interface{})
		if ok {
			es.joinsModels = list
		}
	}
	return es
}

type easyScope struct {
	*gorm.Scope
	sourceScope *gorm.Scope
	forgeSearch *search
	condition   struct {
		SqlKey        string        //sql查询语句
		SqlValue      string        //sql查询值
		SqlCountValue string        //sql查询值
		ObjectField   []string      //使用的模型列
		PrimaryValue  []interface{} //主键查询值
	}
	opt            *option.ModelOption
	valueType      reflect.Type
	Table          string
	joinsModels    []interface{}
	joinsCondition []struct {
		ObjectField []string //使用的模型列
		Table       string   //表名
	}

	primaryFieldName string
	handle           *Handle
}

func (es *easyScope) QueryScope() *easyScope {
	es.condition.SqlKey = es.combinedConditionSql()
	es.condition.SqlKey = strings.ToLower(es.condition.SqlKey)
	es.condition.SqlKey = strings.ReplaceAll(es.condition.SqlKey, "`deleted_at` is null", "")
	for _, field := range es.Fields() {
		if field.IsPrimaryKey {
			continue
		}

		column := strings.ReplaceAll(es.Quote(field.DBName), "`", "")
		if strings.ToLower(column) == "deleted_at" {
			continue
		}
		if !strings.Contains(es.condition.SqlKey, column) {
			continue
		}

		es.condition.ObjectField = append(es.condition.ObjectField, column)
	}
	es.buildJoinsCondition()

	for k, v := range replaceFormat {
		es.condition.SqlKey = strings.ReplaceAll(es.condition.SqlKey, k, v)
	}

	vars := []string{}
	for index := 0; index < len(es.SQLVars); index++ {
		vars = append(vars, fmt.Sprint(es.SQLVars[index]))
	}

	es.condition.SqlCountValue = strings.Join(vars, "_")
	es.condition.SqlCountValue = strings.ReplaceAll(es.condition.SqlCountValue, " ", "_")
	es.condition.SqlValue = strings.Join(vars, "_") + es.orderSQL() + es.limitAndOffsetSQL()
	es.condition.SqlValue = strings.ReplaceAll(es.condition.SqlValue, " ", "_")
	return es
}

func (es *easyScope) buildJoinsCondition() {
	for index := 0; index < len(es.joinsModels); index++ {
		mScope := es.handle.db.NewScope(es.joinsModels[index])
		var item struct {
			ObjectField []string //使用的模型列
			Table       string   //表名
		}
		item.Table = mScope.TableName()

		for _, field := range mScope.Fields() {
			if field.IsPrimaryKey {
				continue
			}
			column := strings.ReplaceAll(mScope.Quote(field.DBName), "`", "")
			if strings.ToLower(column) == "deleted_at" {
				continue
			}

			if strings.Contains(es.condition.SqlKey, column) || strings.Contains(es.condition.SqlKey, item.Table+"."+column) {
				item.ObjectField = append(item.ObjectField, column)
			}
		}

		if len(item.ObjectField) == 0 {
			panic("Columns that do not use this model.")
		}

		es.joinsCondition = append(es.joinsCondition, item)
	}

	joinTable := ""
	for _, c := range es.forgeSearch.joinConditions {
		joinTable += fmt.Sprint(c["query"])
	}
	if joinTable != "" {
		es.condition.SqlKey = es.condition.SqlKey + "joins_" + joinTable
	}
}

func (es *easyScope) buildSelectQuery(clause map[string]interface{}) (str string) {
	switch value := clause["query"].(type) {
	case string:
		str = value
	case []string:
		str = strings.Join(value, ", ")
	}

	args := clause["args"].([]interface{})
	replacements := []string{}
	for _, arg := range args {
		switch reflect.ValueOf(arg).Kind() {
		case reflect.Slice:
			values := reflect.ValueOf(arg)
			var tempMarks []string
			for i := 0; i < values.Len(); i++ {
				tempMarks = append(tempMarks, es.AddToVars(values.Index(i).Interface()))
			}
			replacements = append(replacements, strings.Join(tempMarks, ","))
		default:
			if valuer, ok := interface{}(arg).(driver.Valuer); ok {
				arg, _ = valuer.Value()
			}
			replacements = append(replacements, es.AddToVars(arg))
		}
	}

	buff := bytes.NewBuffer([]byte{})
	i := 0
	for pos, char := range str {
		if str[pos] == '?' {
			buff.WriteString(replacements[i])
			i++
		} else {
			buff.WriteRune(char)
		}
	}

	str = buff.String()
	return
}

func (es *easyScope) whereSQL() (sql string) {
	var (
		deletedAtField, hasDeletedAtField              = es.FieldByName("DeletedAt")
		primaryConditions, andConditions, orConditions []string
	)

	if !es.forgeSearch.Unscoped && hasDeletedAtField {
		sql := fmt.Sprintf("%v IS NULL", es.Quote(deletedAtField.DBName))
		primaryConditions = append(primaryConditions, sql)
	}

	if !es.PrimaryKeyZero() {
		for _, field := range es.PrimaryFields() {
			sql := fmt.Sprintf("%v = %v", es.Quote(field.DBName), es.AddToVars(field.Field.Interface()))
			primaryConditions = append(primaryConditions, sql)
		}
	}

	for _, clause := range es.forgeSearch.whereConditions {
		//获取主键
		if sql := es.buildCondition(clause, true); sql != "" {
			andConditions = append(andConditions, sql)
		}
	}

	for _, clause := range es.forgeSearch.orConditions {
		if sql := es.buildCondition(clause, true); sql != "" {
			orConditions = append(orConditions, sql)
		}
	}

	for _, clause := range es.forgeSearch.notConditions {
		if sql := es.buildCondition(clause, false); sql != "" {
			andConditions = append(andConditions, sql)
		}
	}

	orSQL := strings.Join(orConditions, " OR ")
	combinedSQL := strings.Join(andConditions, " AND ")
	if len(combinedSQL) > 0 {
		if len(orSQL) > 0 {
			combinedSQL = combinedSQL + " OR " + orSQL
		}
	} else {
		combinedSQL = orSQL
	}

	if len(primaryConditions) > 0 {
		sql = strings.Join(primaryConditions, " AND ")
		if len(combinedSQL) > 0 {
			sql = sql + " AND (" + combinedSQL + ")"
		}
	} else if len(combinedSQL) > 0 {
		sql = combinedSQL
	}
	return
}

func (es *easyScope) buildCondition(clause map[string]interface{}, include bool) (str string) {
	var (
		quotedPrimaryKey = es.Quote(es.PrimaryKey())
		equalSQL         = "="
		inSQL            = "IN"
	)

	// If building not conditions
	if !include {
		equalSQL = "<>"
		inSQL = "NOT IN"
	}

	switch value := clause["query"].(type) {
	case sql.NullInt64:
		return fmt.Sprintf("(%v %s %v)", quotedPrimaryKey, equalSQL, value.Int64)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		es.condition.PrimaryValue = []interface{}{value}
		return fmt.Sprintf("(%v %s %v)", quotedPrimaryKey, equalSQL, value)
	case []int, []int8, []int16, []int32, []int64, []uint, []uint8, []uint16, []uint32, []uint64, []string, []interface{}:
		if !include && reflect.ValueOf(value).Len() == 0 {
			return
		}
		str = fmt.Sprintf("(%v %s (?))", quotedPrimaryKey, inSQL)
		clause["args"] = []interface{}{value}
		rvalue := reflect.ValueOf(value)
		for index := 0; index < rvalue.Len(); index++ {
			es.condition.PrimaryValue = append(es.condition.PrimaryValue, rvalue.Index(index).Interface())
		}
	case string:
		_, ok := es.Get("gorm:association:source")
		if ok {
			if value == "`"+es.PrimaryKey()+"` = ?" {
				argsv, ok := clause["args"].([]interface{})
				if ok && len(argsv) > 0 {
					es.condition.PrimaryValue = append(es.condition.PrimaryValue, argsv[0])
				}

			}
		}
		if isNumberRegexp.MatchString(value) {
			es.condition.PrimaryValue = []interface{}{value}
			return fmt.Sprintf("(%v %s %v)", quotedPrimaryKey, equalSQL, es.AddToVars(value))
		}

		if value != "" {
			if !es.IsCompleteParentheses(value) {
				es.Err(fmt.Errorf("incomplete parentheses found: %v", value))
				return
			}
			if !include {
				if comparisonRegexp.MatchString(value) {
					str = fmt.Sprintf("NOT (%v)", value)
				} else {
					str = fmt.Sprintf("(%v NOT IN (?))", es.Quote(value))
				}
			} else {
				str = fmt.Sprintf("(%v)", value)
			}
		}
	case map[string]interface{}:
		var sqls []string
		for key, value := range value {
			if value != nil {
				sqls = append(sqls, fmt.Sprintf("(%v %s %v)", es.Quote(key), equalSQL, es.AddToVars(value)))
			} else {
				if !include {
					sqls = append(sqls, fmt.Sprintf("(%v IS NOT NULL)", es.Quote(key)))
				} else {
					sqls = append(sqls, fmt.Sprintf("(%v IS NULL)", es.Quote(key)))
				}
			}
		}
		return strings.Join(sqls, " AND ")
	case interface{}:
		var sqls []string
		newScope := es.New(value)

		if len(newScope.Fields()) == 0 {
			es.Err(fmt.Errorf("invalid query condition: %v", value))
			return
		}
		scopeQuotedTableName := newScope.QuotedTableName()
		for _, field := range newScope.Fields() {
			if !field.IsIgnored && !field.IsBlank {
				sqls = append(sqls, fmt.Sprintf("(%v.%v %s %v)", scopeQuotedTableName, es.Quote(field.DBName), equalSQL, es.AddToVars(field.Field.Interface())))
			}
		}
		return strings.Join(sqls, " AND ")
	default:
		es.Err(fmt.Errorf("invalid query condition: %v", value))
		return
	}

	replacements := []string{}
	args := clause["args"].([]interface{})
	for _, arg := range args {
		var err error
		switch reflect.ValueOf(arg).Kind() {
		case reflect.Slice: // For where("id in (?)", []int64{1,2})
			if scanner, ok := interface{}(arg).(driver.Valuer); ok {
				arg, err = scanner.Value()
				replacements = append(replacements, es.AddToVars(arg))
			} else if b, ok := arg.([]byte); ok {
				replacements = append(replacements, es.AddToVars(b))
			} else if as, ok := arg.([][]interface{}); ok {
				var tempMarks []string
				for _, a := range as {
					var arrayMarks []string
					for _, v := range a {
						arrayMarks = append(arrayMarks, es.AddToVars(v))
					}

					if len(arrayMarks) > 0 {
						tempMarks = append(tempMarks, fmt.Sprintf("(%v)", strings.Join(arrayMarks, ",")))
					}
				}

				if len(tempMarks) > 0 {
					replacements = append(replacements, strings.Join(tempMarks, ","))
				}
			} else if values := reflect.ValueOf(arg); values.Len() > 0 {
				var tempMarks []string
				for i := 0; i < values.Len(); i++ {
					tempMarks = append(tempMarks, es.AddToVars(values.Index(i).Interface()))
				}
				replacements = append(replacements, strings.Join(tempMarks, ","))
			} else {
				replacements = append(replacements, es.AddToVars(gorm.Expr("NULL")))
			}
		default:
			if valuer, ok := interface{}(arg).(driver.Valuer); ok {
				arg, err = valuer.Value()
			}

			replacements = append(replacements, es.AddToVars(arg))
		}

		if err != nil {
			es.Err(err)
		}
	}

	buff := bytes.NewBuffer([]byte{})
	i := 0
	for _, s := range str {
		if s == '?' && len(replacements) > i {
			buff.WriteString(replacements[i])
			i++
		} else {
			buff.WriteRune(s)
		}
	}

	str = buff.String()

	return
}

func (es *easyScope) optionSetting() {
	if es.Value == nil {
		return
	}

	var structValue reflect.Value
	refvalue := reflect.ValueOf(es.Value)
	if refvalue.Kind() == reflect.Ptr {
		refvalue = refvalue.Elem()
	}
	if refvalue.Kind() == reflect.Slice {
		structValue = reflect.New(refvalue.Type().Elem())
	} else {
		structValue = refvalue
	}

	if structValue.Kind() == reflect.Ptr {
		structValue = structValue.Elem()
	}
	es.valueType = structValue.Type()

	ccall := structValue.Addr().MethodByName("Cache")
	if ccall.IsValid() {
		ccall.Call([]reflect.Value{reflect.ValueOf(es.opt)})
	}

	if es.opt.Expires < option.MinExpires {
		es.opt.Expires = option.MinExpires
	}
	if es.opt.Expires > option.MaxExpires {
		es.opt.Expires = option.MaxExpires
	}
}

func (es *easyScope) limitAndOffsetSQL() string {
	return es.Dialect().LimitAndOffsetSQL(es.forgeSearch.limit, es.forgeSearch.offset)
}

func (es *easyScope) combinedConditionSql() string {
	whereSQL := es.whereSQL()
	return whereSQL
}

func (es *easyScope) quoteIfPossible(str string) string {
	if columnRegexp.MatchString(str) {
		return es.Quote(str)
	}
	return str
}

func (es *easyScope) EasyCount() (int, error) {
	var count int
	err := es.sourceScope.DB().InstantSet("cache:easy_count", true).Count(&count).Error
	return count, err
}

// Primarys 任意条件 获取主键列表
func (es *easyScope) EasyPrimarys() (primarys []interface{}, err error) {
	value := reflect.MakeSlice(reflect.SliceOf(es.valueType), 0, 0)
	value = reflect.New(value.Type())
	newScope := es.sourceScope.DB().NewScope(value.Interface())
	newScope.Search.Select(es.Table + "." + es.PrimaryKey())
	newScope.Search.Limit(es.forgeSearch.limit)
	query := newScope.DB().Callback().Query().Get("gorm:query")
	query(newScope)
	preload := newScope.DB().Callback().Query().Get("gorm:preload")
	preload(newScope)
	after_query := newScope.DB().Callback().Query().Get("gorm:after_query")
	after_query(newScope)
	err = newScope.DB().Error
	if err != nil {
		return
	}
	rows := value.Elem()
	pkFieldName := es.PrimaryFieldName()
	for index := 0; index < rows.Len(); index++ {
		row := rows.Index(index)
		pk := row.FieldByName(pkFieldName).Interface()
		if pk == nil {
			continue
		}
		primarys = append(primarys, pk)
	}
	return
}

type expr struct {
	expr string
	args []interface{}
}

func (es *easyScope) orderSQL() string {
	if len(es.forgeSearch.orders) == 0 || es.forgeSearch.ignoreOrderQuery {
		return ""
	}

	var orders []string
	for _, order := range es.forgeSearch.orders {
		orders = append(orders, fmt.Sprint(order))
		continue
		fmt.Println("order:", order)
		if str, ok := order.(string); ok {
			orders = append(orders, es.quoteIfPossible(str))
		} else if expr, ok := order.(*expr); ok {
			exp := expr.expr
			for _, arg := range expr.args {
				exp = strings.Replace(exp, "?", es.AddToVars(arg), 1)
			}
			orders = append(orders, exp)
		}
	}
	return strings.Join(orders, "_")
}

func (es *easyScope) groupSQL() string {
	if len(es.forgeSearch.group) == 0 {
		return ""
	}
	return " GROUP BY " + es.forgeSearch.group
}

func (es *easyScope) havingSQL() string {
	if len(es.forgeSearch.havingConditions) == 0 {
		return ""
	}

	var andConditions []string
	for _, clause := range es.forgeSearch.havingConditions {
		if sql := es.buildCondition(clause, true); sql != "" {
			andConditions = append(andConditions, sql)
		}
	}

	combinedSQL := strings.Join(andConditions, " AND ")
	if len(combinedSQL) == 0 {
		return ""
	}

	return " HAVING " + combinedSQL
}

func (es *easyScope) PrimaryFieldName() string {
	return es.primaryFieldName
}

// isJoinSkip
func (es *easyScope) isJoinSkip() bool {
	if len(es.forgeSearch.joinConditions) > 0 && len(es.joinsModels) == 0 {
		return true
	}
	return false
}

// isSelectSkip
func (es *easyScope) isSelectSkip() bool {
	if es.forgeSearch.selects != nil && len(es.forgeSearch.selects) > 0 {
		return true
	}

	for _, value := range es.forgeSearch.whereConditions {
		sql := strings.ToLower(fmt.Sprint(value["query"]))
		if strings.Contains(sql, "select") && len(es.joinsModels) == 0 {
			return true
		}
	}

	return false
}

var (
	columnRegexp        = regexp.MustCompile("^[a-zA-Z\\d]+(\\.[a-zA-Z\\d]+)*$")
	isNumberRegexp      = regexp.MustCompile("^\\s*\\d+\\s*$")
	comparisonRegexp    = regexp.MustCompile("(?i) (=|<>|(>|<)(=?)|LIKE|IS|IN) ")
	countingQueryRegexp = regexp.MustCompile("(?i)^count(.+)$")
	replaceFormat       = map[string]string{
		"and": "&",
		"or":  "|",
		" ":   "",
		"`":   "",
		"$$$": "$",
	}
)