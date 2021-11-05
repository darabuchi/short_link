package main

import (
	"encoding/base64"
	"fmt"
	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/darabuchi/utils"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cache"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/pterm/pterm"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	})
	app.Server().Logger = log.New()

	app.Server().ErrorHandler = func(ctx *fasthttp.RequestCtx, err error) {
		log.Errorf("%s err:%v", ctx.Request.String(), err)
	}

	app.Use(
		func(ctx *fiber.Ctx) error {
			start := time.Now()
			err := ctx.Next()
			ctx.Set("X-Handle-Time", time.Since(start).String())
			return err
		},
		cors.New(cors.Config{
			AllowOrigins:     "*",
			AllowMethods:     "*",
			AllowHeaders:     "*",
			AllowCredentials: false,
			ExposeHeaders:    "*",
		}),
		cache.New(cache.Config{
			Next: func(c *fiber.Ctx) bool {
				refresh := c.Query("refresh")
				return refresh == "1" || strings.ToLower(refresh) == "true"
			},
			Expiration:   time.Minute * 5,
			CacheControl: true,
			//Storage:      storage,
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

	app.Get("/:hash", func(ctx *fiber.Ctx) error {
		var short ShortMap
		err = db.Where("token = ?", ctx.Params("hash")).First(&short).Error
		if err != nil {
			if err == gorm.ErrRecordNotFound {
				ctx.Status(400)
				return ctx.SendString("not found url")
			}
			log.Errorf("err:%v", err)
			return err
		}

		return ctx.Redirect(short.JumpUrl, 301)
	})

	app.Post("/short", func(ctx *fiber.Ctx) error {
		var req struct {
			Url string `json:"url" yaml:"url"`
		}

		var rsp struct {
			Code     int    `json:"Code"`
			Message  string `json:"Message"`
			LongUrl  string `json:"LongUrl"`
			ShortUrl string `json:"ShortUrl"`
		}

		err = ctx.BodyParser(&req)
		if err != nil {
			log.Errorf("err:%v", err)
			return err
		}

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
					Token:   utils.ShortStr(utils.Sha512(string(u)), 10),
					JumpUrl: string(u),
				}
				err = db.Where("token = ?", short.Token).FirstOrCreate(&short).Error
				if err != nil {
					log.Errorf("err:%v", err)
					return err
				}

				rsp.Code = 0
				rsp.ShortUrl = ctx.BaseURL() + "/" + short.Token
			}
		}

		return ctx.JSONP(rsp)
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
