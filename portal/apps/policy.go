// Copyright (c) 2015-2022 CloudJ Technology Co., Ltd.

package apps

import (
	"cloudiac/common"
	"cloudiac/policy"
	"cloudiac/portal/consts"
	"cloudiac/portal/consts/e"
	"cloudiac/portal/libs/ctx"
	"cloudiac/portal/libs/db"
	"cloudiac/portal/libs/page"
	"cloudiac/portal/models"
	"cloudiac/portal/models/forms"
	"cloudiac/portal/services"
	"cloudiac/portal/services/logstorage"
	"cloudiac/utils"
	"cloudiac/utils/logs"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// 发起执行多个云模版的合规检测任务
func ScanTemplates(c *ctx.ServiceContext, form *forms.ScanTemplateForms) ([]*models.ScanTask, e.Error) {
	scanTasks := []*models.ScanTask{}
	for _, v := range form.Ids {
		tplForm := &forms.ScanTemplateForm{
			Id: v,
		}
		scanTask, err := ScanTemplateOrEnv(c, tplForm, "")
		if err != nil {
			return nil, err
		}
		scanTasks = append(scanTasks, scanTask)
	}
	return scanTasks, nil
}

// ScanTemplateOrEnv 扫描云模板或环境的合规策略
func ScanTemplateOrEnv(c *ctx.ServiceContext, form *forms.ScanTemplateForm, envId models.Id) (*models.ScanTask, e.Error) {
	c.AddLogField("action", fmt.Sprintf("scan template %s", form.Id))
	if envId != "" {
		c.AddLogField("envId", envId.String())
	}

	tx := c.Tx()
	txWithOrg := services.QueryWithOrgIdAndGlobal(tx, c.OrgId)
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()

	var (
		tpl       *models.Template
		env       *models.Env
		err       e.Error
		projectId models.Id
	)

	if envId != "" { // 环境检查
		env, err = IsScanableEnv(txWithOrg, envId, form.Parse)
		if err != nil {
			return nil, err
		}
		projectId = env.ProjectId
	}

	// 模板检查
	if tpl, err = IsScanableTpl(tx, form.Id, envId, form.Parse); err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	// 创建任务
	runnerId, err := services.GetDefaultRunnerId()
	if err != nil {
		_ = tx.Rollback()
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	// 确定任务类型
	taskType := GetScanTaskType(envId, form.Parse)

	var task *models.ScanTask
	if envId != "" {
		task, err = services.CreateEnvScanTask(txWithOrg, tpl, env, taskType, c.UserId)
	} else {
		task, err = services.CreateScanTask(txWithOrg, tpl, env, models.ScanTask{
			Name:      models.ScanTask{}.GetTaskNameByType(taskType),
			OrgId:     c.OrgId,
			CreatorId: c.UserId,
			TplId:     tpl.Id,
			EnvId:     envId,
			ProjectId: projectId,
			BaseTask: models.BaseTask{
				Type:        taskType,
				StepTimeout: common.DefaultTaskStepTimeout,
				RunnerId:    runnerId,
			},
		})
	}

	if err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("error creating scan task, err %s", err)
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}

	if err := services.InitScanResult(tx, task); err != nil {
		return nil, e.New(e.DBError, errors.Wrapf(err, "task '%s' init scan result", task.Id))
	}

	if err := UpdateLastScanTaskId(tx, task, env, tpl); err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("save last scan task id err %s", err)
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("commit env, err %s", err)
		return nil, e.New(e.DBError, err)
	}
	return task, nil
}

func GetScanTaskType(envId models.Id, parseOnly bool) string {
	if envId != "" && !parseOnly {
		return models.TaskTypeEnvScan
	} else if envId != "" && parseOnly {
		return models.TaskTypeEnvParse
	} else if envId == "" && !parseOnly {
		return models.TaskTypeTplScan
	} else {
		// envId == "" && form.Parse
		return models.TaskTypeTplParse
	}
}

func IsScanableEnv(tx *db.Session, envId models.Id, isParse bool) (*models.Env, e.Error) {
	env, err := services.GetEnvById(tx, envId)
	if err != nil && err.Code() == e.EnvNotExists {
		_ = tx.Rollback()
		return nil, e.New(err.Code(), err, http.StatusBadRequest)
	} else if err != nil {
		_ = tx.Rollback()
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	// env 状态检查
	if env.Archived {
		return nil, e.New(e.EnvArchived, http.StatusBadRequest)
	}

	// 环境扫描未启用，不允许发起手动检测
	if enabled, err := services.IsEnvEnabledScan(tx, envId); err != nil {
		_ = tx.Rollback()
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	} else if !enabled && !isParse {
		_ = tx.Rollback()
		return nil, e.New(e.PolicyScanNotEnabled, http.StatusBadRequest)
	}

	return env, nil
}

func IsScanableTpl(tx *db.Session, tplId models.Id, envId models.Id, isParse bool) (*models.Template, e.Error) {
	tpl, err := services.GetTemplateById(tx, tplId)
	if err != nil && err.Code() == e.TemplateNotExists {
		return nil, e.New(err.Code(), err, http.StatusBadRequest)
	} else if err != nil {
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	if tpl.Status == models.Disable {
		return nil, e.New(e.TemplateDisabled, http.StatusBadRequest)
	}

	if envId == "" {
		// 云模板扫描未启用，不允许发起手动检测
		if enabled, err := services.IsTemplateEnabledScan(tx, tplId); err != nil {
			_ = tx.Rollback()
			return nil, e.New(e.DBError, err, http.StatusInternalServerError)
		} else if !enabled && !isParse {
			_ = tx.Rollback()
			return nil, e.New(e.PolicyScanNotEnabled, http.StatusBadRequest)
		}
	}
	return tpl, nil
}

func UpdateLastScanTaskId(tx *db.Session, task *models.ScanTask, env *models.Env, tpl *models.Template) e.Error {
	if task.Type == models.TaskTypeEnvScan {
		env.LastScanTaskId = task.Id
		if _, err := tx.Save(env); err != nil {
			return e.New(e.DBError, err, http.StatusInternalServerError)
		}
	} else if task.Type == models.TaskTypeTplScan {
		tpl.LastScanTaskId = task.Id
		if _, err := tx.Save(tpl); err != nil {
			return e.New(e.DBError, err, http.StatusInternalServerError)
		}
	}
	return nil
}

// ScanEnvironment 扫描环境策略
func ScanEnvironment(c *ctx.ServiceContext, form *forms.ScanEnvironmentForm) (*models.ScanTask, e.Error) {
	c.AddLogField("action", fmt.Sprintf("scan environment %s", form.Id))
	if c.OrgId == "" {
		return nil, e.New(e.BadRequest, http.StatusBadRequest)
	}

	tx := c.Tx()
	txWithOrg := services.QueryWithOrgIdAndGlobal(c.Tx(), c.OrgId)
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()

	envQuery := services.QueryWithOrgId(tx, c.OrgId)
	env, err := IsScanableEnv(envQuery, form.Id, false)
	if err != nil {
		return nil, err
	}

	// 模板检查
	tplQuery := services.QueryWithOrgId(txWithOrg, c.OrgId)
	tpl, err := IsScanableTpl(tplQuery, env.TplId, form.Id, false)
	if err != nil {
		return nil, err
	}

	// 确定任务类型
	taskType := GetScanTaskType(form.Id, false)

	task, err := services.CreateEnvScanTask(tx, tpl, env, taskType, c.UserId)
	if err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("error creating scan task, err %s", err)
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}

	env.LastScanTaskId = task.Id
	if _, err := tx.Save(env); err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("save env, err %s", err)
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	if err := services.InitScanResult(tx, task); err != nil {
		_ = tx.Rollback()
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("commit env, err %s", err)
		return nil, e.New(e.DBError, err)
	}
	return task, nil
}

type PolicyResp struct {
	models.Policy
	GroupName string `json:"groupName"`
	Creator   string `json:"creator"`
	Summary
}

// SearchPolicy 查询策略列表
func SearchPolicy(c *ctx.ServiceContext, form *forms.SearchPolicyForm) (interface{}, e.Error) {
	query := services.SearchPolicy(c.DB(), form, c.OrgId)
	policyResps := make([]PolicyResp, 0)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	if err := p.Scan(&policyResps); err != nil {
		return nil, e.New(e.DBError, err)
	}

	// 扫描结果统计信息
	var policyIds []models.Id
	for idx := range policyResps {
		policyIds = append(policyIds, policyResps[idx].Id)
	}
	if summaries, err := services.PolicySummary(c.DB(), policyIds, consts.ScopePolicy, c.OrgId); err != nil { //nolint
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	} else if len(summaries) > 0 {
		sumMap := make(map[string]*services.PolicyScanSummary, len(policyIds))
		for idx, summary := range summaries {
			sumMap[string(summary.Id)+summary.Status] = summaries[idx]
		}
		for idx, policyResp := range policyResps {
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusPassed]; ok {
				policyResps[idx].Passed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusViolated]; ok {
				policyResps[idx].Violated = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusFailed]; ok {
				policyResps[idx].Failed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusSuppressed]; ok {
				policyResps[idx].Suppressed = summary.Count
			}
		}
	}

	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     policyResps,
	}, nil
}

// DetailPolicy 查询策略组详情
func DetailPolicy(c *ctx.ServiceContext, form *forms.DetailPolicyForm) (interface{}, e.Error) {
	query := services.QueryWithOrgId(c.DB(), c.OrgId)
	return services.DetailPolicy(query, form.Id)
}

type RespPolicyTpl struct {
	models.Template

	PolicyStatus string `json:"policyStatus"` // 策略检查状态, enum('passed','violated','pending','failed')

	PolicyGroups []services.NewPolicyGroup `json:"policyGroups" gorm:"-"`
	OrgName      string                    `json:"orgName" form:"orgName" `
	Summary

	// 以下字段不返回
	Status string `json:"status,omitempty" gorm:"-" swaggerignore:"true"` // 模板状态(enabled, disable)
}

func SearchPolicyTpl(c *ctx.ServiceContext, form *forms.SearchPolicyTplForm) (interface{}, e.Error) {
	respPolicyTpls := make([]*RespPolicyTpl, 0)
	tplIds := make([]models.Id, 0)
	query := services.SearchPolicyTpl(c.DB(), c.UserId, c.OrgId, form.TplId, form.Q)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	groupM := make(map[models.Id][]services.NewPolicyGroup)
	if err := p.Scan(&respPolicyTpls); err != nil {
		return nil, e.New(e.DBError, err)
	}
	for _, v := range respPolicyTpls {
		tplIds = append(tplIds, v.Id)
		v.PolicyStatus = models.PolicyStatusConversion(v.PolicyStatus, v.PolicyEnable)
	}

	// 根据模板id查询出关联的所有策略组
	groups, err := services.GetPolicyGroupByTplIds(c.DB(), tplIds)
	if err != nil {
		return nil, err
	}
	for _, v := range groups {
		if _, ok := groupM[v.TplId]; !ok {
			groupM[v.TplId] = []services.NewPolicyGroup{v}
			continue
		}
		groupM[v.TplId] = append(groupM[v.TplId], v)
	}

	for index, v := range respPolicyTpls {
		if _, ok := groupM[v.Id]; !ok {
			respPolicyTpls[index].PolicyGroups = []services.NewPolicyGroup{}
			continue
		}
		respPolicyTpls[index].PolicyGroups = groupM[v.Id]
	}

	summaries, err := services.PolicyTargetSummary(c.DB(), tplIds, consts.ScopeTemplate)
	if err != nil {
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     PolicyTargetSummaryTpl(respPolicyTpls, summaries),
	}, nil
}

type RespPolicyEnv struct {
	models.Env

	PolicyStatus string `json:"policyStatus"` // 策略检查状态, enum('passed','violated','pending','failed')

	PolicyGroups []services.NewPolicyGroup `json:"policyGroups" gorm:"-"`
	Summary
	OrgName      string `json:"orgName" form:"orgName" `
	ProjectName  string `json:"projectName" form:"projectName" `
	TemplateName string `json:"templateName"`

	// 以下字段不返回
	Status     string `json:"status,omitempty" gorm:"-" swaggerignore:"true"`     // 环境状态
	TaskStatus string `json:"taskStatus,omitempty" gorm:"-" swaggerignore:"true"` // 环境部署任务状态
}

func SearchPolicyEnv(c *ctx.ServiceContext, form *forms.SearchPolicyEnvForm) (interface{}, e.Error) {
	respPolicyEnvs := make([]*RespPolicyEnv, 0)
	envIds := make([]models.Id, 0)
	query := services.SearchPolicyEnv(c.DB(), c.UserId, c.OrgId, form.ProjectId, form.EnvId, form.Q)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))

	if err := p.Scan(&respPolicyEnvs); err != nil {
		return nil, e.New(e.DBError, err)
	}
	for _, v := range respPolicyEnvs {
		v.PolicyStatus = models.PolicyStatusConversion(v.PolicyStatus, v.PolicyEnable)
	}

	for _, env := range respPolicyEnvs {
		envIds = append(envIds, env.Id)
	}

	// 根据环境id查询出关联的所有策略组
	groups, err := services.GetPolicyGroupByEnvIds(c.DB(), envIds)
	if err != nil {
		return nil, err
	}

	groupM := make(map[models.Id][]services.NewPolicyGroup)
	for _, v := range groups {
		if _, ok := groupM[v.EnvId]; !ok {
			groupM[v.EnvId] = []services.NewPolicyGroup{}
		}
		groupM[v.EnvId] = append(groupM[v.EnvId], v)
	}

	for index, v := range respPolicyEnvs {
		if _, ok := groupM[v.Id]; !ok {
			respPolicyEnvs[index].PolicyGroups = []services.NewPolicyGroup{}
		} else {
			respPolicyEnvs[index].PolicyGroups = groupM[v.Id]
		}
	}

	summaries, err := services.PolicyTargetSummary(c.DB(), envIds, consts.ScopeEnv)
	if err != nil {
		return nil, e.New(e.DBError, err, http.StatusInternalServerError)
	}

	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     PolicyTargetSummaryEnv(respPolicyEnvs, summaries),
	}, nil
}

type RespEnvOfPolicy struct {
	models.Policy
	GroupName string `json:"groupName"`
	GroupId   string `json:"groupId"`
	EnvName   string `json:"envName"`
}

func EnvOfPolicy(c *ctx.ServiceContext, form *forms.EnvOfPolicyForm) (interface{}, e.Error) {
	resp := make([]RespEnvOfPolicy, 0)
	query := services.EnvOfPolicy(c.DB(), form, c.OrgId, c.ProjectId)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	if err := p.Scan(&resp); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     resp,
	}, nil
}

type ValidPolicyResp struct {
	ValidPolicies      []models.Policy `json:"validPolicies"`
	SuppressedPolicies []models.Policy `json:"suppressedPolicies"`
}

func ValidEnvOfPolicy(c *ctx.ServiceContext, form *forms.EnvOfPolicyForm) (interface{}, e.Error) {
	validPolicies, suppressedPolicies, err := services.GetValidPolicies(c.DB(), "", form.Id)
	if err != nil {
		return nil, err
	}
	return ValidPolicyResp{
		ValidPolicies:      validPolicies,
		SuppressedPolicies: suppressedPolicies,
	}, nil
}

type RespTplOfPolicy struct {
	models.Policy
	GroupName string `json:"groupName"`
	GroupId   string `json:"groupId"`
	TplName   string `json:"tplName"`
}

func TplOfPolicy(c *ctx.ServiceContext, form *forms.TplOfPolicyForm) (interface{}, e.Error) {
	resp := make([]RespTplOfPolicy, 0)
	query := services.TplOfPolicy(c.DB(), form, c.OrgId, c.ProjectId)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	if err := p.Scan(&resp); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     resp,
	}, nil
}

type RespTplOfPolicyGroup struct {
	GroupName string `json:"groupName"`
	GroupId   string `json:"groupId"`
}

func TplOfPolicyGroup(c *ctx.ServiceContext, form *forms.TplOfPolicyGroupForm) (interface{}, e.Error) {
	resp := make([]RespTplOfPolicyGroup, 0)
	query := services.QueryWithOrgId(c.DB(), c.OrgId, models.PolicyGroup{}.TableName())
	query = services.TplOfPolicyGroup(query, form)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	if err := p.Scan(&resp); err != nil {
		return nil, e.New(e.DBError, err)
	}
	return page.PageResp{
		Total:    p.MustTotal(),
		PageSize: p.Size,
		List:     resp,
	}, nil
}

func ValidTplOfPolicy(c *ctx.ServiceContext, form *forms.TplOfPolicyForm) (interface{}, e.Error) {
	validPolicies, suppressedPolicies, err := services.GetValidPolicies(c.DB(), form.Id, "")
	if err != nil {
		return nil, err
	}
	return ValidPolicyResp{
		ValidPolicies:      validPolicies,
		SuppressedPolicies: suppressedPolicies,
	}, nil
}

type PolicyErrorResp struct {
	models.PolicyResult
	TargetId     models.Id `json:"targetId"`
	EnvName      string    `json:"envName"`
	TemplateName string    `json:"templateName"`
}

func (PolicyErrorResp) TableName() string {
	return models.PolicyResult{}.TableName()
}

// PolicyError 获取合规错误列表，包含最后一次检测错误和合规不通过，排除已经屏蔽的条目
func PolicyError(c *ctx.ServiceContext, form *forms.PolicyErrorForm) (interface{}, e.Error) {
	query := services.QueryWithOrgId(c.DB(), c.OrgId, models.PolicyResult{}.TableName())
	query = services.PolicyError(query, form.Id)
	if form.HasKey("q") {
		query = query.Where(fmt.Sprintf("env_name LIKE '%%%s%%' or template_name LIKE '%%%s%%'", form.Q, form.Q))
	}
	return getPage(query, form, PolicyErrorResp{})
}

type ParseResp struct {
	Template *services.TfParse `json:"template"`
}

// ParseTemplate 解析云模板/环境源码
func ParseTemplate(c *ctx.ServiceContext, form *forms.PolicyParseForm) (interface{}, e.Error) {
	c.AddLogField("action", fmt.Sprintf("parse template %s env %s", form.TemplateId, form.EnvId))
	query := services.QueryWithOrgId(c.DB(), c.OrgId)

	tplId := form.TemplateId
	envId := models.Id("")
	if form.HasKey("envId") {
		env, err := services.GetEnvById(query, form.EnvId)
		if err != nil {
			return nil, e.New(err.Code(), err, http.StatusBadRequest)
		}
		tplId = env.TplId
		envId = env.Id
	}

	f := forms.ScanTemplateForm{
		Id:    tplId,
		Parse: true,
	}
	scanTask, err := ScanTemplateOrEnv(c, &f, envId)
	if err != nil {
		return nil, err
	}

	ticker := time.NewTicker(time.Second)
	timeout := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer timeout.Stop()

	// 等待任务执行完成
	for {
		scanTask, err = services.GetScanTaskById(query, scanTask.Id)
		if err != nil {
			return nil, e.New(e.PolicyErrorParseTemplate, fmt.Errorf("parse tempalte error: %+v", err), http.StatusInternalServerError)
		}
		if scanTask.IsExitedStatus(scanTask.Status) {
			break
		}

		select {
		case <-ticker.C:
			continue
		case <-timeout.C:
			return nil, e.New(e.PolicyErrorParseTemplate, fmt.Errorf("parse tempalte timeout"), http.StatusInternalServerError)
		}
	}

	if scanTask.Status == common.TaskComplete {
		content, er := logstorage.Get().Read(scanTask.TfParseJsonPath())
		if er != nil {
			return nil, e.New(e.PolicyErrorParseTemplate, fmt.Errorf("parse tempalte error: %v", err), http.StatusInternalServerError)
		}
		js, err := services.UnmarshalTfParseJson(content)
		if err != nil {
			return nil, e.New(e.PolicyErrorParseTemplate, fmt.Errorf("parse tempalte error: %v", err), http.StatusInternalServerError)
		}
		return ParseResp{
			Template: js,
		}, nil
	}
	return nil, e.New(e.PolicyErrorParseTemplate, fmt.Errorf("execute parse tempalte error: %v", err), http.StatusInternalServerError)
}

type ScanResultPageResp struct {
	PolicyStatus string               `json:"policyStatus"` // 扫描状态
	Task         *models.ScanTask     `json:"task"`         // 扫描任务
	Total        int64                `json:"total"`        // 总数
	PageSize     int                  `json:"pageSize"`     // 分页数量
	List         []*PolicyResultGroup `json:"groups"`       // 策略组
}

type PolicyResultGroup struct {
	Id      models.Id      `json:"id"`
	Name    string         `json:"name"`
	Summary Summary        `json:"summary"`
	List    []PolicyResult `json:"list"` // 策略扫描结果
}

type PolicyResult struct {
	models.PolicyResult
	PolicyName      string `json:"policyName" example:"VPC 安全组规则"`  // 策略名称
	PolicySuppress  bool   `json:"policySuppress"`                  //是否屏蔽
	PolicyGroupName string `json:"policyGroupName" example:"安全策略组"` // 策略组名称
	FixSuggestion   string `json:"fixSuggestion" example:"建议您创建一个专有网络..."`
	Rego            string `json:"rego" example:""` //rego 代码文件内容
}

func checkScopeEnabled(query *db.Session, scope string, id models.Id) (bool, e.Error) {

	if scope == consts.ScopeEnv {
		return services.IsEnvEnabledScan(query, id)
	} else if scope == consts.ScopeTemplate {
		return services.IsTemplateEnabledScan(query, id)
	} else {
		return false, e.New(e.BadParam, fmt.Errorf("unknown policy scan result scope '%s'", scope))
	}
}

func getLastScanTaskByScope(query *db.Session, scope string, id models.Id) (*models.ScanTask, e.Error) {
	scanTask, err := services.GetLastScanTaskByScope(query, scope, id)
	if err != nil {
		if e.IsRecordNotFound(err) {
			if scope == consts.ScopeEnv {
				return nil, e.AutoNew(err, e.EnvNotExists)
			} else if scope == consts.ScopeTemplate {
				return nil, e.AutoNew(err, e.TemplateNotExists)
			}
		}
		return nil, e.AutoNew(err, e.DBError)
	}
	return scanTask, nil
}

func getScanTaskVarious(query *db.Session, taskId models.Id, scope string, id models.Id) (*models.ScanTask, e.Error) {
	if taskId != "" {
		scanTask, err := services.GetScanTaskById(query, taskId)
		if err != nil {
			if err.Code() == e.TaskNotExists {
				return nil, e.New(e.ObjectNotExists)
			}
			return nil, e.AutoNew(err, e.DBError)
		}
		return scanTask, nil
	} else {
		scanTask, err := getLastScanTaskByScope(query, scope, id)
		if err != nil {
			if err.Code() == e.EnvNotExists || err.Code() == e.TemplateNotExists {
				return nil, e.New(e.ObjectNotExists)
			}
			return nil, e.AutoNew(err, e.DBError)
		}
		return scanTask, nil
	}
}

func groupByGroup(results []PolicyResult) []*PolicyResultGroup {
	lastGroup := &PolicyResultGroup{}
	var resultGroups []*PolicyResultGroup
	for i, r := range results {
		if lastGroup.Name != r.PolicyGroupName {
			lastGroup = &PolicyResultGroup{
				Id:   r.PolicyGroupId,
				Name: r.PolicyGroupName,
			}
			resultGroups = append(resultGroups, lastGroup)
		}
		lastGroup.List = append(lastGroup.List, results[i])
		switch r.Status {
		case common.PolicyStatusPassed:
			lastGroup.Summary.Passed++
		case common.PolicyStatusViolated:
			lastGroup.Summary.Violated++
		case common.PolicyStatusFailed:
			lastGroup.Summary.Failed++
		case common.PolicyStatusSuppressed:
			lastGroup.Summary.Suppressed++
		}
	}
	return resultGroups
}

func PolicyScanResult(c *ctx.ServiceContext, scope string, form *forms.PolicyScanResultForm) (interface{}, e.Error) {
	c.AddLogField("action", fmt.Sprintf("scan result for %s:%s %s", scope, form.Id, form.TaskId))

	query := services.QueryWithOrgId(c.DB(), c.OrgId)

	policyEnable, _ := checkScopeEnabled(query, scope, form.Id)
	scanTask, err := getScanTaskVarious(query, form.TaskId, scope, form.Id)
	if err != nil {
		if err.Code() == e.ObjectNotExists {
			// 默认返回空列表
			emptyResult := ScanResultPageResp{
				PolicyStatus: services.MergeScanResultPolicyStatus(policyEnable, nil),
			}
			return emptyResult, nil
		}
		return nil, err
	}

	// 如果正在扫描中，返回空列表
	if scanTask.PolicyStatus == common.TaskPending {
		return ScanResultPageResp{
			PolicyStatus: services.MergeScanResultPolicyStatus(policyEnable, scanTask),
			Task:         scanTask,
			Total:        0,
			PageSize:     0,
			List:         []*PolicyResultGroup{},
		}, nil
	}

	query = services.QueryWithOrgId(c.DB(), c.OrgId, models.PolicyResult{}.TableName())
	query = services.QueryPolicyResult(query, scanTask.Id)
	query = services.QueryPolicySuppress(query, scope, form.Id)
	if form.SortField() == "" {
		query = query.Order("policy_group_name, policy_name")
	} else {
		// 优先分组排序，再做分组内排序
		query = query.Order(fmt.Sprintf("policy_group_name, %s %s", form.SortField(), form.SortOrder()))
	}
	results := make([]PolicyResult, 0)
	p := page.New(form.CurrentPage(), form.PageSize(), form.Order(query))
	if err := p.Scan(&results); err != nil {
		return nil, e.New(e.DBError, err)
	}

	// 按策略组分组
	resultGroups := groupByGroup(results)

	return ScanResultPageResp{
		PolicyStatus: services.MergeScanResultPolicyStatus(policyEnable, scanTask),
		Task:         scanTask,
		Total:        p.MustTotal(),
		PageSize:     p.Size,
		List:         resultGroups,
	}, nil
}

type Summary struct {
	Passed     int `json:"passed"`
	Violated   int `json:"violated"`
	Suppressed int `json:"suppressed"`
	Failed     int `json:"failed"`
}

type Polyline struct {
	Column []string `json:"column,omitempty" example:"08-20,08-21"`
	Value  []int    `json:"value,omitempty" example:"101,103"`
}

type PieChar []PieSector

type PieSector struct {
	Name  string `json:"name" example:"08-20"`
	Value int    `json:"value" example:"10"`
}

type PolicyScanReportResp struct {
	Total            PieChar         `json:"total"`            // 检测结果比例
	TaskScanCount    Polyline        `json:"scanCount"`        // 检测源执行次数
	PolicyScanCount  Polyline        `json:"policyScanCount"`  // 策略运行趋势
	PolicyPassedRate PolylinePercent `json:"policyPassedRate"` // 检测通过率趋势
}

func PolicyScanReport(c *ctx.ServiceContext, form *forms.PolicyScanReportForm) (*PolicyScanReportResp, e.Error) { //nolint:cyclop
	if !form.HasKey("showCount") {
		// 默认展示最近五个
		form.ShowCount = 5
	}
	if !form.HasKey("to") {
		form.To = time.Now()
	}
	if !form.HasKey("from") {
		// 默认展示近 5 天的数据
		form.From = utils.LastDaysMidnight(5, form.To)
	}

	timePoints := make([]string, 0)
	{
		start := form.From
		for !start.After(form.To) {
			_, m, d := start.Date()
			timePoints = append(timePoints, fmt.Sprintf("%02d-%02d", m, d))
			start = start.AddDate(0, 0, 1)
		}
	}

	query := services.QueryWithOrgId(c.DB(), c.OrgId)
	scanStatus, err := services.GetPolicyScanStatus(query, form.Id, form.From, form.To, consts.ScopePolicy)
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}

	report := PolicyScanReportResp{}
	totalScan := &report.PolicyScanCount
	passedScan := &report.PolicyPassedRate
	totalSummary := Summary{}

	// 初始化返回的时间点序列，保证没有查询到数据的时候点也会自动填充 0 值
	for _, d := range timePoints {
		totalScan.Column = append(totalScan.Column, d)
		totalScan.Value = append(totalScan.Value, 0)

		passedScan.Column = append(passedScan.Column, d)
		passedScan.Value = append(passedScan.Value, 0)
	}

	for _, s := range scanStatus {
		d := s.Date[5:10] // 2021-08-08T00:00:00+08:00 => 08-08
		found := false
		for idx := range totalScan.Column {
			if totalScan.Column[idx] == d {
				// 跳过扫描中状态策略
				if s.Status != common.PolicyStatusPending {
					totalScan.Value[idx] += s.Count
				}
				if s.Status == common.PolicyStatusPassed {
					passedScan.Value[idx] = Percent(s.Count)
				}

				found = true
				break
			}
		}
		if !found {
			c.Logger().Warnf("date '%s' not in time range %v", d, timePoints)
			return nil, e.New(e.InternalError, fmt.Errorf("date '%s' not in time range", d))
		}

		for idx := range totalScan.Column {
			if totalScan.Value[idx] == 0 {
				passedScan.Value[idx] = 0
				continue
			}
			passedScan.Value[idx] = passedScan.Value[idx] / Percent(totalScan.Value[idx])
		}

		switch s.Status {
		case common.PolicyStatusPassed:
			totalSummary.Passed += s.Count
		case common.PolicyStatusViolated:
			totalSummary.Violated += s.Count
		case common.PolicyStatusSuppressed:
			totalSummary.Suppressed += s.Count
		case common.PolicyStatusFailed:
			totalSummary.Failed += s.Count
		}
	}
	report.Total = append(report.Total, PieSector{
		Name:  common.PolicyStatusPassed,
		Value: totalSummary.Passed,
	}, PieSector{
		Name:  common.PolicyStatusViolated,
		Value: totalSummary.Violated,
	}, PieSector{
		Name:  common.PolicyStatusSuppressed,
		Value: totalSummary.Suppressed,
	}, PieSector{
		Name:  common.PolicyStatusFailed,
		Value: totalSummary.Failed,
	})

	scanTaskStatus, err := services.GetPolicyScanByTarget(c.DB(), form.Id, form.From, form.To, form.ShowCount, c.OrgId)
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	taskCount := &report.TaskScanCount

	for _, s := range scanTaskStatus {
		taskCount.Column = append(taskCount.Column, s.Name)
		taskCount.Value = append(taskCount.Value, s.Count)
	}

	return &report, nil
}

type PolylinePercent struct {
	Column []string  `json:"column,omitempty" example:"08-20,08-21"`
	Value  []Percent `json:"value,omitempty" example:"0.951,0.962"`
	Passed []int     `json:"-"`
	Total  []int     `json:"-"`
}

type Percent float64 // 百分比，保留1位百分比小数，0.951 = 95.1%

func (n Percent) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.3f", n)), nil
}

type PolicyTestResp struct {
	Data         interface{} `json:"data" swaggertype:"string" example:"{\n\"accurics\":{\n\"instanceWithNoVpc\":[\n{\n\"Id\":\"alicloud_instance.instance\"\n}\n]\n}\n}"` // 脚本测试输出，json文本
	Error        string      `json:"error" example:"1 error occurred: policy.rego:4: rego_parse_error: refs cannot be used for rule\n"`                                    // 脚本执行错误内容
	PolicyStatus string      `json:"policyStatus"`
}

func PolicyTest(c *ctx.ServiceContext, form *forms.PolicyTestForm) (*PolicyTestResp, e.Error) {
	c.AddLogField("action", "test template")

	var value interface{}
	if err := json.Unmarshal([]byte(form.Input), &value); err != nil {
		return &PolicyTestResp{
			Data:  map[string]interface{}{},
			Error: fmt.Sprintf("invalid input %v", err),
		}, nil
	}

	tmpDir, err := os.MkdirTemp("", "*")
	if err != nil {
		return nil, e.New(e.InternalError, errors.Wrapf(err, "create tmp dir"), http.StatusInternalServerError)
	}
	defer os.RemoveAll(tmpDir)

	regoPath := filepath.Join(tmpDir, "policy.rego")
	inputPath := filepath.Join(tmpDir, "input.json")

	if err := os.WriteFile(regoPath, []byte(form.Rego), 0644); err != nil { //nolint:gosec
		return nil, e.New(e.InternalError, err, http.StatusInternalServerError)
	}
	if err := os.WriteFile(inputPath, []byte(form.Input), 0644); err != nil { //nolint:gosec
		return nil, e.New(e.InternalError, err, http.StatusInternalServerError)
	}

	if result, err := policy.RegoParse(regoPath, inputPath); err != nil {
		return &PolicyTestResp{
			Data:         map[string]interface{}{},
			Error:        fmt.Sprintf("%s", err),
			PolicyStatus: common.PolicyStatusFailed,
		}, nil
	} else {
		status := common.PolicyStatusPassed
		res := (&policy.Rego{}).ParseResource(result)
		if len(res) > 0 {
			status = common.PolicyStatusViolated
		}
		return &PolicyTestResp{
			Data:         result,
			Error:        "",
			PolicyStatus: status,
		}, nil
	}
}

type PieCharPercent []PieSectorPercent

type PieSectorPercent struct {
	Name  string  `json:"name" example:"passed"`
	Value float64 `json:"value" example:"0.2"`
}

type PolicySummaryResp struct {
	ActivePolicy struct {
		Total   int     `json:"total"`   // 最近 15 天产生扫描记录的策略数量
		Last    int     `json:"last"`    // 16～30 天产生扫描记录的策略数量
		Changes float64 `json:"changes"` // 相较上次变化
		Summary PieChar `json:"summary"` // 策略状态包含
	} `json:"activePolicy"`

	UnresolvedPolicy struct {
		Total   int     `json:"total"`   // 最近 15 天产生扫描记录的策略数量
		Last    int     `json:"last"`    // 16～30 天产生扫描记录的策略数量
		Changes float64 `json:"changes"` // 相较上次变化： (total - last) / last
		Summary PieChar `json:"summary"` // 策略严重级别统计
	} `json:"unresolvedPolicy"`

	PolicyViolated      PieChar `json:"policyViolated"`      // 策略不通过
	PolicyGroupViolated PieChar `json:"policyGroupViolated"` // 策略组不通过
}

func PolicySummary(c *ctx.ServiceContext) (*PolicySummaryResp, e.Error) { //nolint:cyclop
	// 策略概览
	// 默认统计时间范围：最近15天
	// 1. 活跃策略
	//    活跃策略定义：产生扫描记录的策略
	//    last （最近15天）定义：第前30～16天
	//    changes 定义：(total - last) / last
	//    summary: 含 passed / violated / failed / suppressed
	// 2. 未解决错误策略
	//    未解决错误定义：策略在任意一个环境或模板上最后一个扫描结果为 violated 或者 failed
	//    扇形图：按策略的严重级别进行统计
	// 3. 策略检测未通过
	//    未通过定义：策略扫描结果为 violated
	//    柱状图：按未通过次数统计，以策略为纬度，取未通过次数最多的 5 条策略记录
	// 4. 策略组检测未通过
	//    未通过定义：扫描结果含 violated 的策略组
	//    柱状图：按未通过次数统计，以策略组为纬度，取未通过次数最多的 5 条策略组记录

	// 最近 15 天
	to := time.Now()
	from := utils.LastDaysMidnight(15, to)
	// 前 16～30 天
	lastTo := from
	lastFrom := utils.LastDaysMidnight(15, lastTo)

	query := services.QueryWithOrgId(c.DB(), c.OrgId)
	userQuery := query.Model(models.PolicyResult{})

	// 用户项目隔离
	// 1. manager / complianceManager 拥有所有项目读取权限
	// 2. member 只能访问用户已授权的项目资源，包括：
	//    1) 所有已授权项目的环境数据
	//    2) 所有已授权项目关联的所有云模板
	if services.UserHasOrgRole(c.UserId, c.OrgId, consts.OrgRoleMember) {
		tplIds, err := services.GetAvailableTemplateIdsByUserId(c.DB(), c.UserId, c.OrgId)
		if err != nil && !e.IsRecordNotFound(err) {
			return nil, e.New(err.Code(), err, http.StatusInternalServerError)
		}
		projectIds := services.UserProjectIds(c.UserId, c.OrgId)
		if len(tplIds) > 0 {
			userQuery = userQuery.Where("(env_id = '' AND tpl_id in (?)) OR (env_id != '') AND project_id in (?)",
				tplIds, projectIds)
		} else {
			// 一个云模板都没有，返回空结果
			return &PolicySummaryResp{}, nil
		}
	}
	summaryResp := PolicySummaryResp{}

	// 近15天数据
	scanStatus, err := services.GetPolicyStatusByPolicy(query, userQuery, from, to, "")
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	totalPolicyMap := make(map[models.Id]int)
	policyStatusMap := make(map[string]int)
	for _, v := range scanStatus {
		// 计算策略数量
		totalPolicyMap[v.Id] = 1
		// 按状态统计数量
		policyStatusMap[v.Status] += v.Count

		// // 计算未通过错误策略数量
		// if v.Status == common.PolicyStatusViolated {
		// 	unresolvedCountMap[v.Id]++
		// }
	}

	// 16～30天数据
	lastScanStatus, err := services.GetPolicyStatusByPolicy(query, userQuery, lastFrom, lastTo, "")
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	lastPolicyMap := make(map[models.Id]int)
	for _, v := range lastScanStatus {
		// 计算策略数量
		lastPolicyMap[v.Id] = 1
	}

	// 1. 活跃策略
	// c.Logger().Errorf("totalPolicyMap %+v", totalPolicyMap)
	summaryResp.ActivePolicy.Total = len(totalPolicyMap)
	summaryResp.ActivePolicy.Last = len(lastPolicyMap)
	if summaryResp.ActivePolicy.Last != 0 {
		summaryResp.ActivePolicy.Changes =
			(float64(summaryResp.ActivePolicy.Total) - float64(summaryResp.ActivePolicy.Last)) /
				float64(summaryResp.ActivePolicy.Last)
	} else {
		summaryResp.ActivePolicy.Changes = 1
	}

	s := PieChar{}
	s = append(s, PieSector{
		Name:  common.PolicyStatusPassed,
		Value: policyStatusMap[common.PolicyStatusPassed],
	}, PieSector{
		Name:  common.PolicyStatusViolated,
		Value: policyStatusMap[common.PolicyStatusViolated],
	}, PieSector{
		Name:  common.PolicyStatusFailed,
		Value: policyStatusMap[common.PolicyStatusFailed],
	}, PieSector{
		Name:  common.PolicyStatusSuppressed,
		Value: policyStatusMap[common.PolicyStatusSuppressed],
	})
	summaryResp.ActivePolicy.Summary = s

	// 2. 未解决错误策略
	{
		// 最近 15 天
		unresolvedPolicies, err := services.QueryPolicyStatusEveryTargetLastRun(query, userQuery, from, to)
		if err != nil {
			return nil, e.AutoNew(errors.Wrap(err, "QueryPolicyStatusEveryTargetLastRun"), e.DBError)
		}

		// 前一个 15天
		lastUnresolvedPolicies, err := services.QueryPolicyStatusEveryTargetLastRun(query, userQuery, lastFrom, lastTo)
		if err != nil {
			return nil, e.AutoNew(errors.Wrap(err, "QueryPolicyStatusEveryTargetLastRun"), e.DBError)
		}

		summaryResp.UnresolvedPolicy.Total = len(unresolvedPolicies)
		summaryResp.UnresolvedPolicy.Last = len(lastUnresolvedPolicies)
		if summaryResp.UnresolvedPolicy.Last != 0 {
			summaryResp.UnresolvedPolicy.Changes =
				(float64(summaryResp.UnresolvedPolicy.Total) -
					float64(summaryResp.UnresolvedPolicy.Last)) /
					float64(summaryResp.UnresolvedPolicy.Last)
		} else {
			summaryResp.UnresolvedPolicy.Changes = 1
		}
		var high, medium, low int
		for _, v := range unresolvedPolicies {
			switch v.Severity {
			case common.PolicySeverityHigh:
				high++
			case common.PolicySeverityMedium:
				medium++
			case common.PolicySeverityLow:
				low++
			}
		}
		s = PieChar{}
		s = append(s, PieSector{
			Name:  common.PolicySeverityHigh,
			Value: high,
		}, PieSector{
			Name:  common.PolicySeverityMedium,
			Value: medium,
		}, PieSector{
			Name:  common.PolicySeverityLow,
			Value: low,
		})
		summaryResp.UnresolvedPolicy.Summary = s
	}

	// 3. 策略未通过
	violatedScanStatus, err := services.GetPolicyStatusByPolicy(query, userQuery, from, to, common.PolicyStatusViolated)
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	p := PieChar{}
	for i := 0; i < 5 && i < len(violatedScanStatus); i++ {
		p = append(p, PieSector{
			Name:  violatedScanStatus[i].Name,
			Value: violatedScanStatus[i].Count,
		})
	}
	summaryResp.PolicyViolated = p

	// 4. 策略组未通过
	violatedGroupScanStatus, err := services.GetPolicyStatusByPolicyGroup(query, userQuery, from, to, common.PolicyStatusViolated)
	if err != nil {
		return nil, e.New(err.Code(), err, http.StatusInternalServerError)
	}
	p = PieChar{}
	for i := 0; i < 5 && i < len(violatedGroupScanStatus); i++ {
		p = append(p, PieSector{
			Name:  violatedGroupScanStatus[i].Name,
			Value: violatedGroupScanStatus[i].Count,
		})
	}
	summaryResp.PolicyGroupViolated = p

	return &summaryResp, nil
}

// PolicyGroupRepoDownloadAndParse 下载和解析策略组文件
func PolicyGroupRepoDownloadAndParse(g *models.PolicyGroup) ([]*policy.PolicyWithMeta, e.Error) {
	// 1. 生成临时工作目录
	logger := logs.Get()
	tmpDir, er := os.MkdirTemp("", "*")
	if er != nil {
		return nil, e.New(e.InternalError, er, http.StatusInternalServerError)
	}
	defer os.RemoveAll(tmpDir)

	// 2. clone 策略组
	var wg sync.WaitGroup
	result := services.DownloadPolicyGroupResult{Group: g}
	wg.Add(1)
	go services.DownloadPolicyGroup(db.Get(), tmpDir, &result, &wg)
	wg.Wait()
	if result.Error != nil {
		logger.Errorf("error download policy group, err %s", result.Error)
		return nil, result.Error
	}

	// 3. 遍历策略组目录，解析策略文件
	return policy.ParsePolicyGroup(filepath.Join(tmpDir, "code", g.Dir))
}

// policiesUpsert 策略文件同步
func policiesUpsert(tx *db.Session, userId models.Id, orgId models.Id, policyGroup *models.PolicyGroup, policyMetas []*policy.PolicyWithMeta) e.Error {
	// 4. 策略同步

	// 删除仓库中已经不存在的策略
	ops, _ := services.GetPoliciesByGroupId(tx, policyGroup.Id, orgId)
	if len(ops) > 0 {
		for _, oldPolicy := range ops {
			found := false
			for _, newPolicy := range policyMetas {
				if newPolicy.Meta.Name == oldPolicy.Name {
					found = true
				}
			}
			if !found {
				_, er := tx.Delete(oldPolicy)
				if er != nil {
					return e.New(e.DBError, er)
				}
			}
		}
	}

	// 创建/更新策略
	for _, pm := range policyMetas {
		np := models.Policy{
			OrgId:     orgId,
			CreatorId: userId,
			GroupId:   policyGroup.Id,

			Name:          pm.Meta.Name,
			RuleName:      pm.Meta.Name,
			ReferenceId:   pm.Meta.ReferenceId,
			Revision:      pm.Meta.Version,
			FixSuggestion: pm.Meta.FixSuggestion,
			Severity:      pm.Meta.Severity,
			ResourceType:  pm.Meta.ResourceType,
			PolicyType:    pm.Meta.PolicyType,
			Tags:          pm.Meta.Category,

			Rego: pm.Rego,
		}
		// 如果策略已经存在则更新已经存在的策略
		op, _ := services.GetPolicyByName(tx, np.Name, policyGroup.Id, orgId)
		if op != nil {
			np.Id = op.Id
		}
		_, er := tx.Save(&np)

		if er != nil {
			return e.New(e.DBError, er)
		}
	}
	return nil
}

func PolicyTargetSummaryTpl(respPolicyTpls []*RespPolicyTpl, summaries []*services.PolicyScanSummary) []*RespPolicyTpl { //nolint:dupl
	if len(summaries) > 0 {
		sumMap := make(map[string]*services.PolicyScanSummary)
		for idx, summary := range summaries {
			sumMap[string(summary.Id)+summary.Status] = summaries[idx]
		}
		for idx, policyResp := range respPolicyTpls {
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusPassed]; ok {
				respPolicyTpls[idx].Passed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusViolated]; ok {
				respPolicyTpls[idx].Violated = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusFailed]; ok {
				respPolicyTpls[idx].Failed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusSuppressed]; ok {
				respPolicyTpls[idx].Suppressed = summary.Count
			}
		}
	}

	return respPolicyTpls
}

func PolicyTargetSummaryEnv(respPolicyEnvs []*RespPolicyEnv, summaries []*services.PolicyScanSummary) []*RespPolicyEnv { //nolint:dupl
	if len(summaries) > 0 {
		sumMap := make(map[string]*services.PolicyScanSummary)
		for idx, summary := range summaries {
			sumMap[string(summary.Id)+summary.Status] = summaries[idx]
		}
		for idx, policyResp := range respPolicyEnvs {
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusPassed]; ok {
				respPolicyEnvs[idx].Passed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusViolated]; ok {
				respPolicyEnvs[idx].Violated = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusFailed]; ok {
				respPolicyEnvs[idx].Failed = summary.Count
			}
			if summary, ok := sumMap[string(policyResp.Id)+common.PolicyStatusSuppressed]; ok {
				respPolicyEnvs[idx].Suppressed = summary.Count
			}
		}
	}
	return respPolicyEnvs
}
