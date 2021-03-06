package integration

import (
	"fmt"
	"io/ioutil"
	"reflect"
	"strconv"
	"testing"

	"github.com/Foundato/kelon/configs"
	"github.com/stretchr/testify/mock"
	"gopkg.in/yaml.v3"
)

type MockedDatastoreExecuter struct {
	mock.Mock
	counter   int
	responses DBTranslatorResponses
	t         *testing.T
	testName  string
}

func NewMockedDatastoreExecuter(t *testing.T, dbQueriesPath, testName string) *MockedDatastoreExecuter {
	mocked := new(MockedDatastoreExecuter)
	mocked.testName = testName
	mocked.On("Configure", mock.Anything, mock.Anything).Return(nil)
	mocked.On("Execute", mock.Anything, mock.Anything).Return(true, nil)

	mocked.counter = 0
	mocked.t = t

	response := &DBTranslatorResponses{}

	// Open config file
	inputBytes, err := ioutil.ReadFile(dbQueriesPath)
	if err != nil {
		t.Error(err)
		t.FailNow()
	}
	// Parse config from yaml to object
	err = yaml.Unmarshal(inputBytes, response)
	if err != nil {
		t.Error(err)
		t.FailNow()
	}
	mocked.responses = *response
	return mocked
}

func (m *MockedDatastoreExecuter) Execute(statement interface{}, params []interface{}) (bool, error) {
	currentResponse := m.responses.Queries[strconv.Itoa(m.counter)]

	// statement map check for mongo datastores, sql datastores have simple string statement
	if reflect.ValueOf(statement).Kind() == reflect.Map {
		convertedStatement := statement.(map[string]string)
		for key, value := range convertedStatement {
			if currentResponse.Query[key] != value {
				m.t.Errorf("Testname: %s / Count %d / Key %s : Query %s does not match expected result %s", m.testName, m.counter, key, value, currentResponse.Query[key])
				m.t.FailNow()
			}
		}
	} else {
		// convert params slice to single string
		paramsString := ""
		for _, value := range params {
			if paramsString == "" {
				paramsString = value.(string)
			} else {
				paramsString = fmt.Sprintf("%s, %s", paramsString, value.(string))
			}
		}

		// assert statement and params
		if statement != currentResponse.Query["sql"] && paramsString != currentResponse.Params {
			m.t.Errorf("Testname: %s / Count %d : Query %s / %s does not match expected result %s / %s", m.testName, m.counter, statement, paramsString, currentResponse.Query, currentResponse.Params)
			m.t.FailNow()
		}
	}
	m.counter++
	return true, nil
}

func (m *MockedDatastoreExecuter) Configure(appConf *configs.AppConfig, alias string) error {
	args := m.Called(appConf, alias)
	return args.Error(0)
}
