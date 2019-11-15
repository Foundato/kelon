package data

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Foundato/kelon/configs"
	"github.com/Foundato/kelon/internal/pkg/util"
	"github.com/Foundato/kelon/pkg/data"

	// Import mysql dirver
	_ "github.com/go-sql-driver/mysql"
	// import postgres driver
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

var (
	//nolint:gochecknoglobals
	hostKey = "host"
	//nolint:gochecknoglobals
	portKey = "port"
	//nolint:gochecknoglobals
	dbKey = "database"
	//nolint:gochecknoglobals
	userKey = "user"
	//nolint:gochecknoglobals
	pwKey = "password"
)

// Return a new data.Datastore which is able to connect to PostgreSQL and MySQL databases.
func NewSQLDatastore() data.Datastore {
	return &sqlDatastore{
		appConf:    nil,
		alias:      "",
		callOps:    nil,
		configured: false,
	}
}

func (ds *sqlDatastore) Configure(appConf *configs.AppConfig, alias string) error {
	// Exit if already configured
	if ds.configured {
		return nil
	}

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

		for schemaName, schema := range schemas {
			if schema.HasNestedEntities() {
				return errors.Errorf("SqlDatastore: Schema %q in datastore with alias [%s] contains nested entities which is not supported by SQL-Datastores yet!", schemaName, alias)
			}
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
	log.Infof("SqlDatastore [%s] laoded call operands [%s]", alias, callOpsFile)

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
	log.Infof("Configured SqlDatastore [%s]", alias)
	return nil
}

func (ds sqlDatastore) Execute(query *data.Node) (bool, error) {
	if !ds.configured {
		return false, errors.New("SqlDatastore was not configured! Please call Configure(). ")
	}
	log.Debugf("TRANSLATING QUERY: ==================%+v==================", (*query).String())

	// Translate query to into sql statement
	statement := ds.translate(query)
	log.Debugf("EXECUTING STATEMENT: ==================%s==================", statement)

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
			log.Infof("Result row with count %d found! -> ALLOWED", count)
			return true, nil
		}
	}

	log.Infof("No resulting row with count > 0 found! -> DENIED")
	return false, nil
}

func (ds sqlDatastore) translate(input *data.Node) string {
	var query util.SStack
	var selects util.SStack
	var entities util.SStack
	var relations util.SStack
	var joins util.SStack

	var operands util.OpStack

	// Walk input
	(*input).Walk(func(q data.Node) {
		switch v := q.(type) {
		case data.Union:
			// Expected stack:  top -> [Queries...]
			query = query.Push(strings.Join(selects, " UNION "))
			selects = selects[:0]
		case data.Query:
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
					log.Errorf("Error while building Query: Too many relations left to build 1 condition! len(relations) = %d", len(relations))
				}
			}

			//nolint:gosec
			selects = selects.Push(fmt.Sprintf("SELECT count(*) FROM %s%s%s", entity, joinClause, condition))
			joins = joins[:0]
			relations = relations[:0]
		case data.Link:
			// Expected stack: entities-top -> [entities] relations-top -> [relations]
			if len(entities) != len(relations) {
				log.Errorf("Error while creating Link: Entities and relations are not balanced! Lengths are Entities[%d:%d]Relations", len(entities), len(relations))
			}
			for i, entity := range entities {
				joins = joins.Push(fmt.Sprintf(" INNER JOIN %s ON %s", entity, strings.Replace(relations[i], "WHERE", "", 1)))
			}
			entities = entities[:0]
			relations = relations[:0]
		case data.Condition:
			// Expected stack: relations-top -> [singleRelation]
			if len(relations) > 0 {
				var rel string
				relations, rel = relations.Pop()
				//nolint:gosec
				relations = relations.Push(fmt.Sprintf(" WHERE %s", rel))
				log.Debugf("CONDITION: relations |%+v <- TOP", relations)
			}
		case data.Disjunction:
			// Expected stack: relations-top -> [disjunctions ...]
			if len(relations) > 0 {
				relations = relations[:0].Push(fmt.Sprintf("(%s)", strings.Join(query, " OR ")))
				log.Debugf("DISJUNCTION: relations |%+v <- TOP", relations)
			}
		case data.Conjunction:
			// Expected stack: relations-top -> [conjunctions ...]
			if len(relations) > 0 {
				relations = relations[:0].Push(fmt.Sprintf("(%s)", strings.Join(relations, " AND ")))
				log.Debugf("CONJUNCTION: relations |%+v <- TOP", relations)
			}
		case data.Attribute:
			// Expected stack:  top -> [entity, ...]
			var entity string
			entities, entity = entities.Pop()
			operands.AppendToTop(fmt.Sprintf("%s.%s", entity, v.Name))
		case data.Call:
			// Expected stack:  top -> [args..., call-op]
			var ops []string
			operands, ops = operands.Pop()
			op := ops[0]

			// Handle Call
			var nextRel string
			if sqlCallOp, ok := ds.callOps[op]; ok {
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
				log.Debugf("RELATION DONE: relations |%+v <- TOP", relations)
			}
		case data.Operator:
			operands = operands.Push([]string{})
			operands.AppendToTop(v.String())
		case data.Entity:
			schema, entity := ds.findSchemaForEntity(v.String())
			if schema == "public" && ds.appConf.Data.Datastores[ds.alias].Type == "postgres" {
				// Special handle when datastore is postgres and schema is public
				entities = entities.Push(entity.Name)
			} else {
				// Normal case for all entities
				entities = entities.Push(fmt.Sprintf("%s.%s", schema, entity.Name))
			}

		case data.Constant:
			operands.AppendToTop(fmt.Sprintf("'%s'", v.String()))
		default:
			log.Warnf("SqlDatastore: Unexpected input: %T -> %+v", v, v)
		}
	})

	return strings.Join(query, "")
}

func (ds sqlDatastore) findSchemaForEntity(search string) (string, *configs.Entity) {
	// Find custom mapping
	for schema, es := range ds.schemas {
		if found, entity := es.ContainsEntity(search); found {
			return schema, entity
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
