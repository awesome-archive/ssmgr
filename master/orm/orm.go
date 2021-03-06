package orm

import (
	"log"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/mysql"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

func New(dialect, arg string) *gorm.DB {
	db, err := gorm.Open(dialect, arg)
	if err != nil {
		log.Fatal("failed to connect database: ", err.Error())
	}

	// create tables, missing columns and missing indexes
	db.AutoMigrate(&User{}, &Allocation{}, &FlowRecord{}, &VerifyCode{})

	return db
}

type User struct {
	ID    string `gorm:"priamry_key,size:32"`
	Email string `gorm:"not null"`
	Group string `gorm:"not null,DEFAULT:'default'"`
	Time  int64  `gorm:"not null,DEFAULT:current_timestamp"`

	// These fields do not conform with 3NF but for performance just keep them here.
	// Remember to update them once Group is changed.
	QuotaFlow int64 `gorm:"not null"`
	Expired   int64 `gorm:"not null"`
	Disabled  bool  `gorm:"not null"`
}

func (User) TableName() string {
	return "users"
}

// A bug of gorm causes Composite Primary Key for SQLite not working
// ref. https://github.com/jinzhu/gorm/issues/1037

type FlowRecord struct {
	UserID   string `gorm:"priamry_key"`
	ServerID string `gorm:"priamry_key"`
	// Port is omitted since one user cannot have multiple ports on a server
	StartTime int64 `gorm:"priamry_key"`
	Flow      int64 `gorm:"not null"`
}

func (FlowRecord) TableName() string {
	return "flow_record"
}

type VerifyCode struct {
	Email string `gorm:"priamry_key"`
	Code  string `gorm:"not null"`
	Time  int64  `gorm:"not null,DEFAULT:current_timestamp"`
}

func (VerifyCode) TableName() string {
	return "verify_code"
}

// Below tables are for deamon

type Allocation struct {
	UserID   string `gorm:"priamry_key,size:32"`
	ServerID string `gorm:"priamry_key"`
	Port     int    `gorm:"not null,index"`
	Password string `gorm:"not null"`
}

func (Allocation) TableName() string {
	return "allocation"
}
