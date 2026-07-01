package routes

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"shorten-url-fiber-redis-yt/database"
	"shorten-url-fiber-redis-yt/helpers"

	"github.com/asaskevich/govalidator"
	"github.com/go-redis/redis/v8"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

type request struct {
	URL         string        `json:"url"`
	CustomShort string        `json:"short"`
	Expiry      time.Duration `json:"expiry"`
}

type response struct {
	URL             string        `json:"url"`
	CustomShort     string        `json:"short"`
	Expiry          time.Duration `json:"expiry"`
	XRateRemaining  int           `json:"rate_limit"`
	XRateLimitReset time.Duration `json:"rate_limit_reset"`
}

func ShortenURL(c *fiber.Ctx) error {
	// Creates an empty request object
	body := new(request)
	// Automatically detects Content-Type and binds to the struct // Parse incoming JSON request
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "cannot parse JSON",
		})
	}

	// rate limiting
	// Creates a Redis client using database 1
	// Database 1 stores only API request limits
	r2 := database.CreateClient(1)
	defer r2.Close()

	// Check Current API Quota
	// Get remaining requests for this IP
	val, err := r2.Get(database.Ctx, c.IP()).Result()
	fmt.Printf("DEBUG: val=%q err=%v\n", val, err)
	if err == redis.Nil {
		// first request from this IP — set quota with 30 min expiry
		if setErr := r2.Set(database.Ctx, c.IP(), os.Getenv("API_QUOTA"), 30*60*time.Second).Err(); setErr != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "cannot initialize rate limit",
			})
		}
		val = os.Getenv("API_QUOTA")
	} else if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "cannot reach rate limit store",
		})
	}
	// Convert quota from string to integer
	// redis returns "10" convert to 10
	valInt, err := strconv.Atoi(val)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "invalid rate limit value",
		})
	}
	// Check Rate Limit
	// Reject request if API quota has been exhausted
	if valInt <= 0 {
		limit, err := r2.TTL(database.Ctx, c.IP()).Result()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "cannot read rate limit TTL",
			})
		}
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":            "Rate limit exceeded",
			"rate_limit_reset": int(limit.Minutes()) + 1, // +1 so we never return 0
		})
	}

	// validate URL
	// Ensure the provided URL is valid
	if !govalidator.IsURL(body.URL) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": "Invalid URL",
		})
	}

	// prevent localhost loop
	// Prevent Localhost URLs
	// Blocks URLs like localhost / 127.0.0.1
	// Prevent shortening localhost or this application's own domain
	if !helpers.RemoveDomainError(body.URL) {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "haha... nice try",
		})
	}

	// Converts google.com into http://google.com
	// Add HTTP scheme if missing
	body.URL = helpers.EnforceHTTP(body.URL)

	// resolve short ID with collision retry
	var id string
	// Generate a random short ID if the user didn't provide one
	if body.CustomShort == "" {
		// Connect to Redis database used for URL storage
		// Database 0 stores URLs.
		r := database.CreateClient(0)
		defer r.Close()

		const maxAttempts = 5
		for i := 0; i < maxAttempts; i++ {
			// Generate a random 6-character short ID
			candidate := uuid.New().String()[:6]
			// Ensure the generated short ID is unique
			existing, err := r.Get(database.Ctx, candidate).Result()
			if err == redis.Nil {
				// key is free
				id = candidate
				break
			} else if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error": "cannot reach URL store",
				})
			} else if existing != "" {
				continue // collision — try again
			}
		}
		if id == "" {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "could not generate a unique short URL, please try again",
			})
		}
	} else {
		id = body.CustomShort
		r := database.CreateClient(0)
		defer r.Close()

		val, err := r.Get(database.Ctx, id).Result()
		if err != nil && err != redis.Nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "cannot reach URL store",
			})
		}
		if val != "" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error": "URL short already in use",
			})
		}
	}

	// Use a default expiry time of 24 hours
	if body.Expiry == 0 {
		body.Expiry = 24
	}

	r := database.CreateClient(0)
	defer r.Close()

	// Save the short URL mapping in Redis with an expiry time
	if err := r.Set(database.Ctx, id, body.URL, body.Expiry*3600*time.Second).Err(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "Unable to connect to server",
		})
	}

	// decrement quota and read remaining
	// Reduce the remaining request quota for this IP
	if err := r2.Decr(database.Ctx, c.IP()).Err(); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "cannot update rate limit",
		})
	}

	remaining, err := r2.Get(database.Ctx, c.IP()).Result()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "cannot read rate limit",
		})
	}
	remainingInt, _ := strconv.Atoi(remaining)

	ttl, err := r2.TTL(database.Ctx, c.IP()).Result()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "cannot read rate limit TTL",
		})
	}

	return c.Status(fiber.StatusOK).JSON(response{
		URL:             body.URL,
		CustomShort:     os.Getenv("DOMAIN") + "/" + id,
		Expiry:          body.Expiry,
		XRateRemaining:  remainingInt,
		XRateLimitReset: ttl / time.Minute, // fixed: Duration/Duration = integer minutes
	})
}
