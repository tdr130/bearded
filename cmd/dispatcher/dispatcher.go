package dispatcher

import (
	"log"
	"net/http"
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/negroni"
	restful "github.com/emicklei/go-restful"
	"github.com/m0sth8/cli" // use fork until subcommands will be fixed
	mgo "gopkg.in/mgo.v2"

	"github.com/bearded-web/bearded/pkg/filters"
	"github.com/bearded-web/bearded/pkg/manager"
	"github.com/bearded-web/bearded/pkg/passlib"
	"github.com/bearded-web/bearded/pkg/scheduler"
	"github.com/bearded-web/bearded/services"
	"github.com/bearded-web/bearded/services/agent"
	"github.com/bearded-web/bearded/services/auth"
	"github.com/bearded-web/bearded/services/me"
	"github.com/bearded-web/bearded/services/plan"
	"github.com/bearded-web/bearded/services/plugin"
	"github.com/bearded-web/bearded/services/project"
	"github.com/bearded-web/bearded/services/scan"
	"github.com/bearded-web/bearded/services/target"
	"github.com/bearded-web/bearded/services/user"
)

var Dispatcher = cli.Command{
	Name:   "dispatcher",
	Usage:  "Start Dispatcher",
	Action: dispatcherAction,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:   "bind-addr",
			Value:  "127.0.0.1:3003",
			EnvVar: "BEARDED_BIND_ADDR",
			Usage:  "http address for binding api server",
		},
		cli.StringFlag{
			Name:   "mongo-addr",
			Value:  "127.0.0.1",
			EnvVar: "BEARDED_MONGO_ADDR",
			Usage:  MongoUsage,
		},
		cli.StringFlag{
			Name:   "mongo-db",
			Value:  "bearded",
			EnvVar: "BEARDED_MONGO_DB",
			Usage:  "Mongodb database",
		},
		cli.StringFlag{
			Name:   "frontend",
			Value:  "../frontend/dist/",
			EnvVar: "BEARDED_FRONTEND",
			Usage:  "path to frontend to serve static",
		},
		cli.BoolFlag{
			Name:   "frontend-off",
			EnvVar: "BEARDED_FRONTEND_OFF",
			Usage:  "do not serve frontend files",
		},
		cli.BoolFlag{
			Name:	"with-agent",
			Usage:	"Run agent inside the dispatcher",
		},
	},
}

func init() {
	Dispatcher.Flags = append(Dispatcher.Flags, swaggerFlags()...)
}

func initServices(wsContainer *restful.Container, db *mgo.Database) error {
	// manager
	mgr := manager.New(db)
	if err := mgr.Init(); err != nil {
		return err
	}

	// password manager for generation and verification passwords
	passCtx := passlib.NewContext()

	sch := scheduler.NewMemoryScheduler(mgr.Copy())

	// services
	base := services.New(mgr, passCtx, sch)
	all := []services.ServiceInterface{
		auth.New(base),
		plugin.New(base),
		plan.New(base),
		user.New(base),
		project.New(base),
		target.New(base),
		scan.New(base),
		me.New(base),
		agent.New(base),
	}

	// initialize services
	for _, s := range all {
		if err := s.Init(); err != nil {
			return err
		}
	}
	// register services in container
	for _, s := range all {
		s.Register(wsContainer)
	}

	return nil
}

//type MgoLogger struct {
//}
//
//func (m *MgoLogger) Output(calldepth int, s string) error {
//	logrus.Debug(s)
//	return nil
//}

func dispatcherAction(ctx *cli.Context) {
	if ctx.GlobalBool("debug") {
		logrus.Info("Debug mode is enabled")
	}

	// initialize mongodb session
	mongoAddr := ctx.String("mongo-addr")
	logrus.Infof("Init mongodb on %s", mongoAddr)
	session, err := mgo.Dial(mongoAddr)
	if err != nil {
		panic(err)
	}
	defer session.Close()
	logrus.Infof("Successfull")
	dbName := ctx.String("mongo-db")
	logrus.Infof("Set mongo database %s", dbName)

	if ctx.GlobalBool("debug") {
		//		mgo.SetLogger(&MgoLogger{)
		//		mgo.SetDebug(true)

		// see what happens inside the package restful
		// TODO (m0sth8): set output to logrus
		restful.TraceLogger(log.New(os.Stdout, "[restful] ", log.LstdFlags|log.Lshortfile))

	}

	// Create container and initialize services
	wsContainer := restful.NewContainer()
	wsContainer.Router(restful.CurlyRouter{}) // CurlyRouter is the faster routing alternative for restful

	// setup session
	cookieOpts := &filters.CookieOpts{
		Path:     "/api/",
		HttpOnly: true,
		//		Secure: true,
	}
	// TODO (m0sth8): extract keys to configuration file
	hashKey := []byte("12345678901234567890123456789012")
	encKey := []byte("12345678901234567890123456789012")
	wsContainer.Filter(filters.SessionCookieFilter("bearded-sss", cookieOpts, hashKey, encKey))

	wsContainer.Filter(filters.MongoFilter(session)) // Add mongo session copy to context on every request
	wsContainer.DoNotRecover(true)                   // Disable recovering in restful cause we recover all panics in negroni

	// Initialize and register services in container
	err = initServices(wsContainer, session.DB(dbName))
	if err != nil {
		panic(err)
	}

	// Swagger should be initialized after services registration
	if !ctx.Bool("swagger-disabled") {
		services.Swagger(wsContainer,
			ctx.String("swagger-api-path"),
			ctx.String("swagger-path"),
			ctx.String("swagger-filepath"))
	}

	// We user negroni as middleware framework.
	app := negroni.New()
	recovery := negroni.NewRecovery() // TODO (m0sth8): create recovery with ServiceError response

	if ctx.GlobalBool("debug") {
		app.Use(negroni.NewLogger())
		// TODO (m0sth8): set output to logrus
		// existed middleware https://github.com/meatballhat/negroni-logrus
	} else {
		recovery.PrintStack = false // do not print stack to response
	}
	app.Use(recovery)

	// TODO (m0sth8): add secure middleware

	if !ctx.Bool("frontend-off") {
		logrus.Infof("Frontend served from %s directory", ctx.String("frontend"))
		app.Use(negroni.NewStatic(http.Dir(ctx.String("frontend"))))
	}

	app.UseHandler(wsContainer) // set wsContainer as main handler

	if ctx.Bool("with-agent") {
		if err := RunInternalAgent(app); err != nil {
			logrus.Error(err)
		}
	}

	// Start negroini middleware with our restful container
	bindAddr := ctx.String("bind-addr")
	server := &http.Server{Addr: bindAddr, Handler: app}
	logrus.Infof("Listening on %s", bindAddr)
	logrus.Fatal(server.ListenAndServe())
}
