package data

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Foundato/kelon/configs"
	"github.com/Foundato/kelon/internal/pkg/util"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type sqlDatastore struct {
	appConf    *configs.AppConfig
	alias      string
	conn       map[string]string
	schemas    map[string]*configs.EntitySchema
	dbPool     *sql.DB
	callOps    map[string]func(args ...string) string
	configured bool
}

var relationOperators = map[string]string{
	"eq":    "=",
	"equal": "=",
	"neq":   "!=",
	"lt":    "<",
	"gt":    ">",
	"lte":   "<=",
	"gte":   ">=",
}

var (
	hostKey = "host"
	portKey = "port"
	dbKey   = "database"
	userKey = "user"
	pwKey   = "password"
)

func NewSqlDatastore() Datastore {
	return &sqlDatastore{
		appConf:    nil,
		alias:      "",
		callOps:    nil,
		configured: false,
	}
}

func (ds *sqlDatastore) Configure(appConf *configs.AppConfig, alias string) error {
	if appConf == nil {
		return errors.New("SqlDatastore: AppConfig not configured! ")
	}
	if alias == "" {
		return errors.New("SqlDatastore: Empty alias provided! ")
	}

	// Validate configuration
	conf, ok := appConf.Data.Datastores[alias]
	if !ok {
		return errors.Errorf("SqlDatastore: No datastore with alias [%s] configured!", alias)
	}
	if strings.ToLower(conf.Type) == "" {
		return errors.Errorf("SqlDatastore: Alias of datastore is empty! Must be one of %+v!", sql.Drivers())
	}
	if err := validateConnection(alias, conf.Connection); err != nil {
		return err
	}
	if schemas, ok := appConf.Data.DatastoreSchemas[alias]; ok {
		if len(schemas) == 0 {
			return errors.Errorf("SqlDatastore: Datastore with alias [%s] has no schemas configured!", alias)
		}
	} else {
		return errors.Errorf("SqlDatastore: Datastore with alias [%s] has no entity-schema-mapping configured!", alias)
	}

	// Init database connection pool
	db, err := sql.Open(conf.Type, getConnectionStringForPlatform(conf.Type, conf.Connection))
	if err != nil {
		return errors.Wrap(err, "SqlDatastore: Error while connecting to database")
	}

	// Ping database for 60 seconds every 3 seconds
	var pingFailure error
	for i := 0; i < 20; i++ {
		if pingFailure = db.Ping(); pingFailure == nil {
			// Ping succeeded
			break
		}
		log.Infof("Waiting for [%s] to be reachable...", alias)
		<-time.After(3 * time.Second)
	}
	if pingFailure != nil {
		return errors.Wrap(err, "SqlDatastore: Unable to ping database")
	}

	// Load call handlers
	callOpsFile := fmt.Sprintf("./call-operands/%s.yml", strings.ToLower(conf.Type))
	handlers, err := LoadDatastoreCallOpsFile(callOpsFile)
	if err != nil {
		return errors.Wrap(err, "SqlDatastore: Unable to load call operands as handlers")
	}
	log.Infof("SqlDatastore [%s] laoded call operands [%s]\n", alias, callOpsFile)

	ds.callOps = map[string]func(args ...string) string{}
	for _, handler := range handlers {
		ds.callOps[handler.Handles()] = handler.Map
	}

	// Assign values
	ds.conn = conf.Connection
	ds.dbPool = db
	ds.schemas = appConf.Data.DatastoreSchemas[alias]
	ds.appConf = appConf
	ds.alias = alias
	ds.configured = true
	log.Infof("Configured SqlDatastore [%s]\n", alias)
	return nil
}

func (ds sqlDatastore) Execute(query *Node) (bool, error) {
	if !ds.configured {
		return false, errors.New("SqlDatastore was not configured! Please call Configure(). ")
	}
	log.Debugf("TRANSLATING QUERY: ==================\n%+v\n ==================", (*query).String())

	// Translate query to into sql statement
	statement, err := ds.translate(query)
	if err != nil {
		return false, errors.New("SqlDatastore: Unable to translate Query!")
	}
	log.Debugf("EXECUTING STATEMENT: ==================\n%s\n ==================", statement)

	rows, err := ds.dbPool.Query(statement)
	if err != nil {
		return false, errors.Wrap(err, "SqlDatastore: Error while executing statement")
	}
	defer func() {
		if err := rows.Close(); err != nil {
			panic("Unable to close Result-Set!")
		}
	}()

	for rows.Next() {
		var count int
		if err := rows.Scan(&count); err != nil {
			return false, errors.Wrap(err, "SqlDatastore: Unable to read result")
		}
		if count > 0 {
			log.Infof("Result row with count %d found! -> ALLOWED\n", count)
			return true, nil
		}
	}

	log.Infof("No resulting row with count > 0 found! -> DENIED")
	return false, nil
}

func (ds sqlDatastore) translate(input *Node) (string, error) {
	var query util.SStack
	var selects util.SStack
	var entities util.SStack
	var relations util.SStack
	var joins util.SStack

	var operands util.OpStack

	// Walk input
	(*input).Walk(func(q Node) {
		switch v := q.(type) {
		case Union:
			// Expected stack:  top -> [Queries...]
			query = query.Push(strings.Join(selects, "\nUNION\n"))
			selects = selects[:0]
		case Query:
			// Expected stack: entities-top -> [singleEntity] relations-top -> [singleCondition]
			var (
				entity     string
				joinClause string
				condition  string
			)
			// Extract entity
			entities, entity = entities.Pop()
			// Extract joins
			for _, j := range joins {
				joinClause += j
			}
			// Extract condition
			if len(relations) > 0 {
				condition = relations[0]
				if len(relations) != 1 {
					log.Errorf("Error while building Query: Too many relations left to build 1 condition! len(relations) = %d\n", len(relations))
				}
			}

			selects = selects.Push(fmt.Sprintf("SELECT count(*) FROM %s%s%s", entity, joinClause, condition))
			joins = joins[:0]
			relations = relations[:0]
		case Link:
			// Expected stack: entities-top -> [entities] relations-top -> [relations]
			if len(entities) != len(relations) {
				log.Errorf("Error while creating Link: Entities and relations are not balanced! Lengths are Entities[%d:%d]Relations\n", len(entities), len(relations))
			}
			for i, entity := range entities {
				joins = joins.Push(fmt.Sprintf("\n\tINNER JOIN %s \n\t\tON %s", entity, strings.Replace(relations[i], "WHERE", "", 1)))
			}
			entities = entities[:0]
			relations = relations[:0]
		case Condition:
			// Expected stack: relations-top -> [singleRelation]
			if len(relations) > 0 {
				var rel string
				relations, rel = relations.Pop()
				relations = relations.Push(fmt.Sprintf("\n\tWHERE \n\t\t%s", rel))
				log.Debugf("CONDITION: relations |%+v <- TOP\n", relations)
			}
		case Disjunction:
			// Expected stack: relations-top -> [disjunctions ...]
			if len(relations) > 0 {
				relations = relations[:0].Push(fmt.Sprintf("(%s)", strings.Join(query, "\n\t\tOR ")))
				log.Debugf("DISJUNCTION: relations |%+v <- TOP\n", relations)
			}
		case Conjunction:
			// Expected stack: relations-top -> [conjunctions ...]
			if len(relations) > 0 {
				relations = relations[:0].Push(fmt.Sprintf("(%s)", strings.Join(relations, "\n\t\tAND ")))
				log.Debugf("CONJUNCTION: relations |%+v <- TOP\n", relations)
			}
		case Attribute:
			// Expected stack:  top -> [entity, ...]
			var entity string
			entities, entity = entities.Pop()
			operands.AppendToTop(fmt.Sprintf("%s.%s", entity, v.Name))
		case Call:
			// Expected stack:  top -> [args..., call-op]
			var ops []string
			operands, ops = operands.Pop()
			op := ops[0]

			// Handle Call
			var nextRel string
			if sqlRelOp, ok := relationOperators[op]; ok {
				// Expected stack:  top -> [rhs, lhs, call-op]
				log.Debugln("NEW RELATION")
				nextRel = fmt.Sprintf("%s %s %s", ops[1], sqlRelOp, ops[2])
			} else if sqlCallOp, ok := ds.callOps[op]; ok {
				// Expected stack:  top -> [args..., call-op]
				log.Debugln("NEW FUNCTION CALL")
				nextRel = sqlCallOp(ops[1:]...)
			} else {
				panic(fmt.Sprintf("Datastores: Operator [%s] is not supported!", op))
			}

			if len(operands) > 0 {
				// If we are in nested call -> push as operand
				operands.AppendToTop(nextRel)
			} else {
				// We reached root operation -> relation is processed
				relations = relations.Push(nextRel)
				log.Debugf("RELATION DONE: relations |%+v <- TOP\n", relations)
			}
		case Operator:
			operands = operands.Push([]string{})
			operands.AppendToTop(v.String())
		case Entity:
			entity := v.String()
			schema := ds.findSchemaForEntity(entity)

			if schema == "public" && ds.appConf.Data.Datastores[ds.alias].Type == "postgres" {
				// Special handle when datastore is postgres and schema is public
				entities = entities.Push(entity)
			} else {
				// Normal case for all entities
				entities = entities.Push(fmt.Sprintf("%s.%s", schema, entity))
			}

		case Constant:
			operands.AppendToTop(fmt.Sprintf("'%s'", v.String()))
		default:
			log.Warnf("SqlDatastore: Unexpected input: %T -> %+v\n", v, v)
		}
	})

	return strings.Join(query, "\n"), nil
}

func (ds sqlDatastore) findSchemaForEntity(search string) string {
	// Find custom mapping
	for schema, es := range ds.schemas {
		for _, entity := range es.Entities {
			if search == entity {
				return schema
			}
		}
	}
	panic(fmt.Sprintf("No schema found for entity %s in datastore with alias %s", search, ds.alias))
}

func validateConnection(alias string, conn map[string]string) error {
	if _, ok := conn[hostKey]; !ok {
		return errors.Errorf("SqlDatastore: Field %s is missing in configured connection with alias %s!", hostKey, alias)
	}
	if _, ok := conn[portKey]; !ok {
		return errors.Errorf("SqlDatastore: Field %s is missing in configured connection with alias %s!", portKey, alias)
	}
	if _, ok := conn[dbKey]; !ok {
		return errors.Errorf("SqlDatastore: Field %s is missing in configured connection with alias %s!", dbKey, alias)
	}
	if _, ok := conn[userKey]; !ok {
		return errors.Errorf("SqlDatastore: Field %s is missing in configured connection with alias %s!", userKey, alias)
	}
	if _, ok := conn[pwKey]; !ok {
		return errors.Errorf("SqlDatastore: Field %s is missing in configured connection with alias %s!", pwKey, alias)
	}
	return nil
}

func getConnectionStringForPlatform(platform string, conn map[string]string) string {
	host := conn[hostKey]
	port := conn[portKey]
	user := conn[userKey]
	password := conn[pwKey]
	dbname := conn[dbKey]

	switch platform {
	case "postgres":
		return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)
	case "mysql":
		return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", user, password, host, port, dbname)
	default:
		panic(fmt.Sprintf("Platform [%s] is not a supported SQL-Datastore!", platform))
	}
}
