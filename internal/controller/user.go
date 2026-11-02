package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// Users lists every console account.
func (a *App) Users(c *echo.Context) error {
	users := make([]model.User, 0)
	if err := a.DB.Order("id ASC").Find(&users).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, users)
}

// CreateUser adds a local password account.
func (a *App) CreateUser(c *echo.Context) error {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeBody(c, &body); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || len(body.Username) > 190 {
		return fail(c, http.StatusBadRequest, "username must contain 1–190 characters")
	}
	if model.RoleRank(body.Role) == 0 {
		return fail(c, http.StatusBadRequest, "role must be admin, editor or viewer")
	}
	hash, err := model.HashPassword(body.Password)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	user := model.User{Username: body.Username, PasswordHash: hash, Role: body.Role}
	if err := a.DB.Create(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return fail(c, http.StatusConflict, "username already exists")
		}
		return internal(c, err)
	}
	return c.JSON(http.StatusCreated, user)
}

// UpdateUser changes a role or resets a local password; a password reset signs the user out everywhere.
func (a *App) UpdateUser(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var body struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
	}
	if err := decodeBody(c, &body); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	var user model.User
	if err := a.DB.First(&user, id).Error; err != nil {
		return persistenceError(c, err)
	}
	updates := map[string]any{}
	if body.Role != nil {
		if model.RoleRank(*body.Role) == 0 {
			return fail(c, http.StatusBadRequest, "role must be admin, editor or viewer")
		}
		// Admins cannot change their own role, so the acting administrator always remains.
		if user.ID == c.Get("cloudpilot.user").(model.User).ID && *body.Role != user.Role {
			return fail(c, http.StatusConflict, "you cannot change your own role")
		}
		updates["role"] = *body.Role
	}
	if body.Password != nil {
		if user.SSO {
			return fail(c, http.StatusConflict, "SSO accounts do not have a password")
		}
		hash, err := model.HashPassword(*body.Password)
		if err != nil {
			return fail(c, http.StatusBadRequest, err.Error())
		}
		updates["password_hash"] = hash
	}
	if len(updates) == 0 {
		return c.JSON(http.StatusOK, user)
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&user).Updates(updates).Error; err != nil {
			return err
		}
		if body.Password != nil {
			return tx.Where("user_id = ?", user.ID).Delete(&model.Session{}).Error
		}
		return nil
	})
	if err != nil {
		return internal(c, err)
	}
	if err := a.DB.First(&user, id).Error; err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, user)
}

// DeleteUser removes another account and its sessions.
func (a *App) DeleteUser(c *echo.Context) error {
	id, err := identifier(c)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	if id == c.Get("cloudpilot.user").(model.User).ID {
		return fail(c, http.StatusConflict, "you cannot delete your own account")
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Delete(&model.User{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return tx.Where("user_id = ?", id).Delete(&model.Session{}).Error
	})
	if err != nil {
		return persistenceError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
