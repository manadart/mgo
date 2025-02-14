package dbtest

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/juju/mgo/v2"
	"github.com/juju/mgo/v2/bson"
	"gopkg.in/tomb.v2"
)

// DBServer controls a MongoDB server process to be used within test suites.
//
// The test server is started when Session is called the first time and should
// remain running for the duration of all tests, with the Wipe method being
// called between tests (before each of them) to clear stored data. After all tests
// are done, the Stop method should be called to stop the test server.
//
// Before the DBServer is used the SetPath method must be called to define
// the location for the database files to be stored.
type DBServer struct {
	session *mgo.Session
	output  bytes.Buffer
	server  *exec.Cmd
	dbpath  string
	replset string
	host    string
	tomb    tomb.Tomb
}

// SetPath defines the path to the directory where the database files will be
// stored if it is started. The directory path itself is not created or removed
// by the test helper.
func (dbs *DBServer) SetPath(dbpath string) {
	dbs.dbpath = dbpath
}

// EnableReplicaset must be called before the database is started. It will
// start the server with '--replSet=<name>' and then call rs.initiate() once
// the server is up and running. Note that mongod startup time is significantly
// slower in replSet mode, but it is necessary for some things like Transaction
// support.
func (dbs *DBServer) EnableReplicaset(name string) {
	dbs.replset = name
}

func (dbs *DBServer) start() {
	if dbs.server != nil {
		panic("DBServer already started")
	}
	if dbs.dbpath == "" {
		panic("DBServer.SetPath must be called before using the server")
	}
	mgo.SetStats(true)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("unable to listen on a local address: " + err.Error())
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	dbs.host = addr.String()

	args := []string{
		"--dbpath", dbs.dbpath,
		"--bind_ip", "127.0.0.1",
		"--port", strconv.Itoa(addr.Port),
	}
	if dbs.replset != "" {
		args = append(args, fmt.Sprintf("--replSet=%s", dbs.replset))
	} else {
		args = append(args, "--nojournal")
	}
	dbs.tomb = tomb.Tomb{}
	dbs.server = exec.Command("mongod", args...)
	dbs.server.Stdout = &dbs.output
	dbs.server.Stderr = &dbs.output
	err = dbs.server.Start()
	if err != nil {
		panic(err)
	}
	dbs.tomb.Go(dbs.monitor)
	if dbs.replset != "" {
		dbs.initiateRepl()
	}
	dbs.Wipe()
}

func (dbs *DBServer) monitor() error {
	dbs.server.Process.Wait()
	if dbs.tomb.Alive() {
		// Present some debugging information.
		fmt.Fprintf(os.Stderr, "---- mongod process died unexpectedly:\n")
		fmt.Fprintf(os.Stderr, "%s", dbs.output.Bytes())
		fmt.Fprintf(os.Stderr, "---- mongod processes running right now:\n")
		cmd := exec.Command("/bin/sh", "-c", "ps auxw | grep mongod")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Run()
		fmt.Fprintf(os.Stderr, "----------------------------------------\n")

		panic("mongod process died unexpectedly")
	}
	return nil
}

func (dbs *DBServer) initiateRepl() {
	session, err := mgo.DialWithInfo(&mgo.DialInfo{
		Addrs:    []string{dbs.host},
		Direct:   true, // must do Direct=true when dealing with a replicaset
		Timeout:  10 * time.Second,
		Database: "test",
		// We don't set ReplicaSetName here, because the replicaset hasn't actually
		// been initialized yet.
		// ReplicaSetName: dbs.replset,
	})
	if err == nil {
		defer session.Close()
		session.SetMode(mgo.Monotonic, true)
		err := session.Run(bson.D{{Name: "replSetInitiate", Value: nil}}, nil)
		if err != nil {
			panic(err)
		}
	} else {
		panic(err)
	}
}

// Stop stops the test server process, if it is running.
//
// It's okay to call Stop multiple times. After the test server is
// stopped it cannot be restarted.
//
// All database sessions must be closed before or while the Stop method
// is running. Otherwise Stop will panic after a timeout informing that
// there is a session leak.
func (dbs *DBServer) Stop() {
	if dbs.session != nil {
		dbs.checkSessions()
		if dbs.session != nil {
			dbs.session.Close()
			dbs.session = nil
		}
	}
	if dbs.server != nil {
		dbs.tomb.Kill(nil)
		dbs.server.Process.Signal(os.Interrupt)
		select {
		case <-dbs.tomb.Dead():
		case <-time.After(5 * time.Second):
			dbs.server.Process.Signal(os.Kill)
			select {
			case <-dbs.tomb.Dead():
			case <-time.After(5 * time.Second):
				panic("timeout waiting for mongod process to die")
			}
		}
		dbs.server = nil
	}
}

// Session returns a new session to the server. The returned session
// must be closed after the test is done with it.
//
// The first Session obtained from a DBServer will start it.
func (dbs *DBServer) Session() *mgo.Session {
	if dbs.server == nil {
		dbs.start()
	}
	if dbs.session == nil {
		mgo.ResetStats()
		var err error
		dbs.session, err = mgo.Dial(dbs.host + "/test")
		if err != nil {
			panic(err)
		}
	}
	return dbs.session.Copy()
}

// checkSessions ensures all mgo sessions opened were properly closed.
// For slightly faster tests, it may be disabled setting the
// environmnet variable CHECK_SESSIONS to 0.
func (dbs *DBServer) checkSessions() {
	if check := os.Getenv("CHECK_SESSIONS"); check == "0" || dbs.server == nil || dbs.session == nil {
		return
	}
	dbs.session.Close()
	dbs.session = nil
	for i := 0; i < 100; i++ {
		stats := mgo.GetStats()
		if stats.SocketsInUse == 0 && stats.SocketsAlive == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("There are mgo sessions still alive.")
}

// Wipe drops all created databases and their data.
//
// The MongoDB server remains running if it was prevoiusly running,
// or stopped if it was previously stopped.
//
// All database sessions must be closed before or while the Wipe method
// is running. Otherwise Wipe will panic after a timeout informing that
// there is a session leak.
func (dbs *DBServer) Wipe() {
	if dbs.server == nil || dbs.session == nil {
		return
	}
	dbs.checkSessions()
	sessionUnset := dbs.session == nil
	session := dbs.Session()
	defer session.Close()
	if sessionUnset {
		dbs.session.Close()
		dbs.session = nil
	}
	names, err := session.DatabaseNames()
	if err != nil {
		panic(err)
	}
	for _, name := range names {
		switch name {
		case "admin", "local", "config":
		default:
			err = session.DB(name).DropDatabase()
			if err != nil {
				panic(err)
			}
		}
	}
}
