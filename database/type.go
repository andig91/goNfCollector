package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ammario/ipisp"
	"github.com/goNfCollector/configurations"
	"github.com/goNfCollector/debugger"
	"github.com/goNfCollector/location"
	"github.com/goNfCollector/reputation"
	"github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// postgres database struct to
// access to postgres DB
type Postgres struct {
	// postgre host
	Host string `json:"host"`

	// postgres port
	Port int `json:"port"`

	// postgres user
	User string `json:"user"`

	// postgres password
	Password string `json:"password"`

	// postgres db
	DB string `json:"db"`

	// debugger
	Debuuger *debugger.Debugger

	// postgres context
	ctx context.Context

	// gorm db
	db *gorm.DB

	// reputation
	reputations []reputation.Reputation

	// IP2locaion instance
	iplocation *location.IPLocation

	// channel
	ch chan os.Signal

	WaitGroup *sync.WaitGroup

	// check if closed
	closed bool

	// this variable used when object inserted to db
	// in order to prevent multiple query on db
	cachedObjects map[string]interface{}

	// lock and unlock for concurrent write & read to
	// prevent "fatal error: concurrent map read and map write"
	// cachedObjectsMapMutex *sync.RWMutex

	cachedObjectsSyncMap sync.Map

	// total number of pending writes
	pendingWites int

	maxOpen int

	// ASN lookup
	IPISPClient ipisp.Client
	IPISPErr    error
}

// insert to local cache
func (p *Postgres) cachedIt(key string, value interface{}) {

	// lock read
	// p.cachedObjectsMapMutex.RLock()

	if _, ok := p.cachedObjectsSyncMap.Load(key); !ok {
		p.cachedObjectsSyncMap.Store(key, value)
	}

	// if _, ok := p.cachedObjects[key]; !ok {

	// 	// unlock read
	// 	// p.cachedObjectsMapMutex.RUnlock()

	// 	// lock write
	// 	// p.cachedObjectsMapMutex.Lock()

	// 	// set to cache
	// 	p.cachedObjects[key] = value

	// 	// unlock write
	// 	// p.cachedObjectsMapMutex.Unlock()
	// }
}

// return object from cache
func (p *Postgres) getCached(key string) (interface{}, error) {

	if value, ok := p.cachedObjectsSyncMap.Load(key); !ok {
		return nil, errors.New("Not found in the cache")
	} else {
		return value, nil
	}

	// lock read
	// p.cachedObjectsMapMutex.RLock()

	// if value, ok := p.cachedObjects[key]; !ok {
	// 	// unlock read
	// 	// p.cachedObjectsMapMutex.Unlock()
	// 	return nil, errors.New("Not found in the cache")
	// } else {
	// 	// unlock read
	// 	// p.cachedObjectsMapMutex.Unlock()
	// 	return value, nil
	// }

}

// return exporter info
func (p Postgres) String() string {
	return fmt.Sprintf("Postgress %s:%d user:%s db:%s", p.Host, p.Port, p.User, p.DB)
}

// return gorm DB
func (p *Postgres) GetDB() *gorm.DB {
	return p.db
}

// create new instance of influxDB
func New(host, user, pass, db string, ipReputationConf configurations.IpReputation, port int, d *debugger.Debugger, ip2location *location.IPLocation, maxIdle, maxOpen int, maxLifeTime time.Duration) Postgres {

	ctx := context.Background()

	d.Verbose(fmt.Sprintf("connecting to postgres db '%s' on %s:%v using username '%s' ", db, host, port, user), logrus.DebugLevel)

	cached := make(map[string]interface{})

	// connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
	// 	user, pass, host, port, db,
	// )
	// conn, err := pgxpool.Connect(ctx, connStr)

	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%d sslmode=disable",
		host, user, pass, db, port,
	)

	pg_db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		// Logger:          logger.Default.LogMode(logger.Silent),
		CreateBatchSize: 1000,
	})

	if err != nil {
		// fmt.Fprintf(os.Stderr, "Unable to connect to database: %v\n", err)
		// os.Exit(1)
		d.Verbose(fmt.Sprintf("can not connect to postgres db '%s' on %s:%v using username '%s' due to error: %s", db, host, port, user, err.Error()), logrus.ErrorLevel)
		os.Exit(configurations.ERROR_CAN_T_CONNECT_TO_POSTGRES_DB.Int())
	}

	// get sql Generic Interface
	sqlDB, err := pg_db.DB()

	// SetMaxIdleConns sets the maximum number of connections in the idle connection pool.
	sqlDB.SetMaxIdleConns(maxIdle)

	// SetMaxOpenConns sets the maximum number of open connections to the database.
	sqlDB.SetMaxOpenConns(maxOpen)

	// SetConnMaxLifetime sets the maximum amount of time a connection may be reused.
	sqlDB.SetConnMaxLifetime(1 * time.Hour)

	// add reputation kind to reputation array
	var reputs []reputation.Reputation
	rptIpSum, err := reputation.NewIPSum(ipReputationConf.IPSumPath)
	if err == nil {
		reput, err := reputation.New(rptIpSum, d)

		if err == nil {
			reputs = append(reputs, *reput)
		}
	}

	d.Verbose(fmt.Sprintf("new postgres %v:%v db:%v user:%v is initilized", host, port, db, user), logrus.DebugLevel)

	ipisp_client, ipisp_err := ipisp.NewDNSClient()

	// retun influxDB
	p := Postgres{
		Host:     host,
		Port:     port,
		User:     user,
		Password: pass,
		DB:       db,
		Debuuger: d,
		ctx:      ctx,
		db:       pg_db,

		iplocation: ip2location,

		reputations: reputs,

		WaitGroup: &sync.WaitGroup{},

		cachedObjects: cached,
		// cachedObjectsMapMutex: &sync.RWMutex{},

		maxOpen: maxOpen,

		IPISPClient: ipisp_client,
		IPISPErr:    ipisp_err,
	}

	// initialize db
	d.Verbose(fmt.Sprintf("initializing postgres db '%s' on %s:%v using username '%s'", db, host, port, user), logrus.DebugLevel)
	err = p.autoMigrate()
	if err != nil {
		d.Verbose(fmt.Sprintf("can not initialize postgres db '%s' on %s:%v using username '%s' due to error: %s", db, host, port, user, err.Error()), logrus.ErrorLevel)
		os.Exit(configurations.ERROR_CAN_T_INIT_POSTGRES_DB.Int())

	}

	return p

}
