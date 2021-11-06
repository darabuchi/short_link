package main

import (
	"embed"
	"encoding/base64"
	"fmt"
	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/bluele/gcache"
	utils2 "github.com/darabuchi/enputi/utils"
	"github.com/darabuchi/utils"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/template/django"
	"github.com/pterm/pterm"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	Title = "Darabuchi-Short Link"
)

var (
	//go:embed views
	drive embed.FS

	cache = gcache.New(1024 * 8).LRU().Build()
)

func getFileSystem(base string) http.FileSystem {
	fsys, err := fs.Sub(drive, base)
	if err != nil {
		panic(err)
	}

	return http.FS(fsys)
}

func main() {
	// 可能存在的目录
	viper.SetConfigFile("./config.yml")

	// 配置一些默认值
	viper.SetDefault("host", "0.0.0.0:8080")

	err := viper.ReadInConfig()
	if err != nil {
		switch e := err.(type) {
		case viper.ConfigFileNotFoundError:
			pterm.Warning.Printfln("not found conf file, use default")
		case *os.PathError:
			pterm.Warning.Printfln("not find conf file in %s", e.Path)
		default:
			pterm.Error.Printfln("load config fail:%v", err)
			return
		}
	}

	log.SetFormatter(&nested.Formatter{
		FieldsOrder: []string{
			log.FieldKeyTime, log.FieldKeyLevel, log.FieldKeyFile,
			log.FieldKeyFunc, log.FieldKeyMsg,
		},
		TimestampFormat:  time.RFC3339,
		HideKeys:         true,
		NoFieldsSpace:    true,
		NoUppercaseLevel: true,
		TrimMessages:     true,
		CallerFirst:      true,
	})
	log.SetLevel(log.DebugLevel)
	log.SetReportCaller(true)

	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("%s?check_same_thread=false", filepath.ToSlash(filepath.Join(utils.GetExecPath(), ".shortlink.db")))), &gorm.Config{
		SkipDefaultTransaction: true,
		NamingStrategy: &schema.NamingStrategy{
			TablePrefix:   "short_link_",
			SingularTable: true,
		},
		DisableForeignKeyConstraintWhenMigrating: true,

		DisableNestedTransaction: true,

		AllowGlobalUpdate: true,
		PrepareStmt:       true,
	})
	if err != nil {
		log.Errorf("err:%v", err)
		return
	}

	err = db.AutoMigrate(
		&ShortMap{},
	)
	if err != nil {
		log.Errorf("err:%v", err)
		return
	}

	app := fiber.New(fiber.Config{
		ErrorHandler: func(ctx *fiber.Ctx, err error) error {
			ctx.Set("X-Error", err.Error())
			return ctx.Status(500).SendString("短连接服务异常")
		},
		ServerHeader:  "",
		CaseSensitive: true,
		UnescapePath:  true,
		// ETag:                     true,
		ReadTimeout:              time.Minute * 5,
		WriteTimeout:             time.Minute * 5,
		CompressedFileSuffix:     ".gz",
		ProxyHeader:              "",
		DisableDefaultDate:       true,
		DisableHeaderNormalizing: true,
		ReduceMemoryUsage:        true,
		BodyLimit:                -1,
		ReadBufferSize:           1024 * 1024 * 4,
		WriteBufferSize:          1024 * 1024 * 4,
		Views:                    django.NewFileSystem(getFileSystem("views"), ".html"),
	})
	app.Server().Logger = log.New()

	app.Server().ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
		log.Errorf("%s err:%v", ctx.Request.String(), err)
	}

	app.Use(
		func(c *fiber.Ctx) error {
			start := time.Now()
			defer c.Set("X-Time", time.Since(start).String())
			reqId := c.Context().ID()
			msg := fmt.Sprintf("<%v>[%s]%s %v %s %s",
				reqId, utils2.GetRealIpFromCtx(c), c.Method(), c.Path(),
				utils.ShortStr4Web(c.Request().Header.String(), -1),
				utils.ShortStr4Web(string(c.Body()), 400))
			log.Info(msg)

			err := c.Next()

			msg = fmt.Sprintf("<%v>%d %s",
				reqId, c.Response().StatusCode(),
				utils.ShortStr4Web(string(c.Response().Body()), 400))
			if err != nil {
				msg += fmt.Sprintf(" err:%v", err)
				log.Error(msg)
			} else {
				log.Info(msg)
			}
			return err
		},
		cors.New(cors.Config{
			AllowOrigins:     "*",
			AllowMethods:     "*",
			AllowHeaders:     "*",
			AllowCredentials: false,
			ExposeHeaders:    "*",
		}),
		compress.New(compress.Config{
			Level: compress.LevelBestCompression,
		}),
		recover.New(recover.Config{
			Next: func(c *fiber.Ctx) bool {
				_ = c.SendStatus(500)
				return false
			},
			EnableStackTrace: false,
			StackTraceHandler: func(e interface{}) {
				log.Errorf("panic:%v", e)
			},
		}),
		func(ctx *fiber.Ctx) error {
			ctx.Status(200)
			return ctx.Next()
		},
	)

	app.Get("/", func(ctx *fiber.Ctx) error {
		//return ctx.SendString("OK")
		return ctx.Render("index", fiber.Map{
			"Title": Title,
		})
	})

	app.Get("/:hash",
		//cache.New(cache.Config{
		//	Expiration: time.Minute * 5,
		//	KeyGenerator: func(ctx *fiber.Ctx) string {
		//		return ctx.Params("hash")
		//	},
		//}),
		func(ctx *fiber.Ctx) error {
			token := ctx.Params("hash")
			if len(token) != 12 {
				ctx.Status(400)
				return ctx.SendString("not found url")
			}

			ctx.Vary("Origin", "User-Agent", "Origin", "Accept-Encoding", "Accept")

			var jumpUrl string
			val, err := cache.Get(token)
			if err == nil {
				jumpUrl = val.(string)
			} else {
				var short ShortMap
				err = db.Where("token = ?", token).First(&short).Error
				if err != nil {
					if err == gorm.ErrRecordNotFound {
						ctx.Status(400)
						return ctx.SendString("not found url")
					}
					log.Errorf("err:%v", err)
					return err
				}

				u, err := base64.StdEncoding.DecodeString(short.JumpUrl)
				if err != nil {
					log.Errorf("err:%v", err)
					return err
				}

				jumpUrl = string(u)
			}

			log.Info(jumpUrl)
			ctx.Location(jumpUrl)
			ctx.Context().Redirect(jumpUrl, 307)
			return ctx.SendString("")
		})

	app.Post("/short", func(ctx *fiber.Ctx) error {
		var req struct {
			Url string `json:"longUrl" yaml:"longUrl"`
		}

		var rsp struct {
			Code     int    `json:"Code"`
			Message  string `json:"Message"`
			LongUrl  string `json:"LongUrl"`
			ShortUrl string `json:"ShortUrl"`
		}

		if ctx.FormValue("longUrl") != "" {
			req.Url = ctx.FormValue("longUrl")
		} else {
			err = ctx.BodyParser(&req)
			if err != nil {
				log.Errorf("err:%v", err)
				return err
			}
		}

		log.Info(req.Url)

		if req.Url == "" {
			rsp.Code = -1
			rsp.Message = "miss url"
		} else {
			u, err := base64.StdEncoding.DecodeString(req.Url)
			if err != nil {
				rsp.Code = -1
				rsp.Message = "url not base64"
			} else {
				short := ShortMap{
					Token:   utils.ShortStr(utils.Sha512(req.Url), 12),
					JumpUrl: req.Url,
				}
				log.Info(string(u))
				err = db.Where("token = ?", short.Token).FirstOrCreate(&short).Error
				if err != nil {
					log.Errorf("err:%v", err)
					return err
				}

				if short.JumpUrl != string(u) {
					err = db.Model(&short).Where("token = ?", short.Token).Update("jump_url", req.Url).Error
					if err != nil {
						log.Errorf("err:%v", err)
						return err
					}
				}

				rsp.Code = 1
				rsp.ShortUrl = ctx.BaseURL() + "/" + short.Token
				rsp.LongUrl = req.Url
			}
		}

		ctx.Status(200)
		return ctx.JSON(rsp)
	})

	app.Post("/sub/index", func(ctx *fiber.Ctx) error {
		var req struct {
			Url string `json:"url" yaml:"url"`
		}

		err = ctx.BodyParser(&req)
		if err != nil {
			log.Errorf("err:%v", err)
			return err
		}

		if req.Url == "" {
			return ctx.SendString("链接为空")
		}

		req.Url = base64.StdEncoding.EncodeToString([]byte(req.Url))

		short := ShortMap{
			Token:   utils.ShortStr(utils.Sha512(req.Url), 12),
			JumpUrl: req.Url,
		}
		err = db.Where("token = ?", short.Token).FirstOrCreate(&short).Error
		if err != nil {
			log.Errorf("err:%v", err)
			return err
		}

		return ctx.Render("short", fiber.Map{
			"Title":    Title,
			"ShortUrl": ctx.BaseURL() + "/" + short.Token,
		})
	})

	err = app.Listen(viper.GetString("host"))
	if err != nil {
		log.Errorf("err:%v", err)
		os.Exit(1)
	}

	return
}

type ShortMap struct {
	Token   string `json:"token" gorm:"varchar(7);uniqueIndex:idx_token"`
	JumpUrl string `json:"jump_url" gorm:"text"`
}
