// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package config

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"

	"net/http"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils"
	"github.com/mattermost/mattermost-server/utils/fileutils"
)

var (
	termsOfServiceEnabledAndEmpty = model.NewAppError(
		"Config.IsValid",
		"model.config.is_valid.support.custom_terms_of_service_text.app_error",
		nil,
		"",
		http.StatusBadRequest,
	)
)

func SaveConfig(fileName string, config *model.Config) *model.AppError {
	b, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return model.NewAppError("SaveConfig", "utils.config.save_config.saving.app_error",
			map[string]interface{}{"Filename": fileName}, err.Error(), http.StatusBadRequest)
	}

	err = ioutil.WriteFile(fileName, b, 0644)
	if err != nil {
		return model.NewAppError("SaveConfig", "utils.config.save_config.saving.app_error",
			map[string]interface{}{"Filename": fileName}, err.Error(), http.StatusInternalServerError)
	}

	return nil
}

type ConfigWatcher struct {
	watcher *fsnotify.Watcher
	close   chan struct{}
	closed  chan struct{}
}

func NewConfigWatcher(cfgFileName string, f func()) (*ConfigWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create config watcher for file: "+cfgFileName)
	}

	configFile := filepath.Clean(cfgFileName)
	configDir, _ := filepath.Split(configFile)
	watcher.Add(configDir)

	ret := &ConfigWatcher{
		watcher: watcher,
		close:   make(chan struct{}),
		closed:  make(chan struct{}),
	}

	go func() {
		defer close(ret.closed)
		defer watcher.Close()

		for {
			select {
			case event := <-watcher.Events:
				// we only care about the config file
				if filepath.Clean(event.Name) == configFile {
					if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
						mlog.Info(fmt.Sprintf("Config file watcher detected a change reloading %v", cfgFileName))

						if _, _, configReadErr := ReadConfigFile(cfgFileName, true); configReadErr == nil {
							f()
						} else {
							mlog.Error(fmt.Sprintf("Failed to read while watching config file at %v with err=%v", cfgFileName, configReadErr.Error()))
						}
					}
				}
			case err := <-watcher.Errors:
				mlog.Error(fmt.Sprintf("Failed while watching config file at %v with err=%v", cfgFileName, err.Error()))
			case <-ret.close:
				return
			}
		}
	}()

	return ret, nil
}

func (w *ConfigWatcher) Close() {
	close(w.close)
	<-w.closed
}

// EnsureConfigFile will attempt to locate a config file with the given name. If it does not exist,
// it will attempt to locate a default config file, and copy it to a file named fileName in the same
// directory. In either case, the config file path is returned.
func EnsureConfigFile(fileName string) (string, error) {
	if configFile := fileutils.FindConfigFile(fileName); configFile != "" {
		return configFile, nil
	}
	if defaultPath := fileutils.FindConfigFile("default.json"); defaultPath != "" {
		destPath := filepath.Join(filepath.Dir(defaultPath), fileName)
		src, err := os.Open(defaultPath)
		if err != nil {
			return "", err
		}
		defer src.Close()
		dest, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return "", err
		}
		defer dest.Close()
		if _, err := io.Copy(dest, src); err == nil {
			return destPath, nil
		}
	}
	return "", fmt.Errorf("no config file found")
}

// LoadConfig will try to search around for the corresponding config file.  It will search
// /tmp/fileName then attempt ./config/fileName, then ../config/fileName and last it will look at
// fileName.
func LoadConfig(fileName string) (*model.Config, string, map[string]interface{}, *model.AppError) {
	var configPath string

	if fileName != filepath.Base(fileName) {
		configPath = fileName
	} else {
		if path, err := EnsureConfigFile(fileName); err != nil {
			appErr := model.NewAppError("LoadConfig", "utils.config.load_config.opening.panic", map[string]interface{}{"Filename": fileName, "Error": err.Error()}, "", 0)
			return nil, "", nil, appErr
		} else {
			configPath = path
		}
	}

	config, envConfig, err := ReadConfigFile(configPath, true)
	if err != nil {
		appErr := model.NewAppError("LoadConfig", "utils.config.load_config.decoding.panic", map[string]interface{}{"Filename": fileName, "Error": err.Error()}, "", 0)
		return nil, "", nil, appErr
	}

	needSave := config.SqlSettings.AtRestEncryptKey == nil || len(*config.SqlSettings.AtRestEncryptKey) == 0 ||
		config.FileSettings.PublicLinkSalt == nil || len(*config.FileSettings.PublicLinkSalt) == 0 ||
		config.EmailSettings.InviteSalt == nil || len(*config.EmailSettings.InviteSalt) == 0

	config.SetDefaults()

	// Don't treat it as an error right now if custom terms of service are enabled but text is empty.
	// This is because terms of service text will be fetched from database at a later state, but
	// the flag indicating it is enabled is fetched from config file right away.
	if err := config.IsValid(); err != nil && err.Id != termsOfServiceEnabledAndEmpty.Id {
		return nil, "", nil, err
	}

	if needSave {
		if err := SaveConfig(configPath, config); err != nil {
			mlog.Warn(err.Error())
		}
	}

	if err := ValidateLocales(config); err != nil {
		if err := SaveConfig(configPath, config); err != nil {
			mlog.Warn(err.Error())
		}
	}

	if *config.FileSettings.DriverName == model.IMAGE_DRIVER_LOCAL {
		dir := config.FileSettings.Directory
		dirString := *dir
		if len(*dir) > 0 && dirString[len(dirString)-1:] != "/" {
			*config.FileSettings.Directory += "/"
		}
	}

	return config, configPath, envConfig, nil
}

func GenerateClientConfig(c *model.Config, diagnosticId string, license *model.License) map[string]string {
	props := GenerateLimitedClientConfig(c, diagnosticId, license)

	props["SiteURL"] = strings.TrimRight(*c.ServiceSettings.SiteURL, "/")
	props["EnableUserDeactivation"] = strconv.FormatBool(*c.TeamSettings.EnableUserDeactivation)
	props["RestrictDirectMessage"] = *c.TeamSettings.RestrictDirectMessage
	props["EnableXToLeaveChannelsFromLHS"] = strconv.FormatBool(*c.TeamSettings.EnableXToLeaveChannelsFromLHS)
	props["TeammateNameDisplay"] = *c.TeamSettings.TeammateNameDisplay
	props["ExperimentalPrimaryTeam"] = *c.TeamSettings.ExperimentalPrimaryTeam
	props["ExperimentalViewArchivedChannels"] = strconv.FormatBool(*c.TeamSettings.ExperimentalViewArchivedChannels)

	props["EnableOAuthServiceProvider"] = strconv.FormatBool(*c.ServiceSettings.EnableOAuthServiceProvider)
	props["GoogleDeveloperKey"] = *c.ServiceSettings.GoogleDeveloperKey
	props["EnableIncomingWebhooks"] = strconv.FormatBool(*c.ServiceSettings.EnableIncomingWebhooks)
	props["EnableOutgoingWebhooks"] = strconv.FormatBool(*c.ServiceSettings.EnableOutgoingWebhooks)
	props["EnableCommands"] = strconv.FormatBool(*c.ServiceSettings.EnableCommands)
	props["EnablePostUsernameOverride"] = strconv.FormatBool(*c.ServiceSettings.EnablePostUsernameOverride)
	props["EnablePostIconOverride"] = strconv.FormatBool(*c.ServiceSettings.EnablePostIconOverride)
	props["EnableUserAccessTokens"] = strconv.FormatBool(*c.ServiceSettings.EnableUserAccessTokens)
	props["EnableLinkPreviews"] = strconv.FormatBool(*c.ServiceSettings.EnableLinkPreviews)
	props["EnableTesting"] = strconv.FormatBool(*c.ServiceSettings.EnableTesting)
	props["EnableDeveloper"] = strconv.FormatBool(*c.ServiceSettings.EnableDeveloper)
	props["PostEditTimeLimit"] = fmt.Sprintf("%v", *c.ServiceSettings.PostEditTimeLimit)
	props["CloseUnusedDirectMessages"] = strconv.FormatBool(*c.ServiceSettings.CloseUnusedDirectMessages)
	props["EnablePreviewFeatures"] = strconv.FormatBool(*c.ServiceSettings.EnablePreviewFeatures)
	props["EnableTutorial"] = strconv.FormatBool(*c.ServiceSettings.EnableTutorial)
	props["ExperimentalEnableDefaultChannelLeaveJoinMessages"] = strconv.FormatBool(*c.ServiceSettings.ExperimentalEnableDefaultChannelLeaveJoinMessages)
	props["ExperimentalGroupUnreadChannels"] = *c.ServiceSettings.ExperimentalGroupUnreadChannels
	props["ExperimentalEnablePostMetadata"] = strconv.FormatBool(*c.ExperimentalSettings.EnablePostMetadata)

	if *c.ServiceSettings.ExperimentalChannelOrganization || *c.ServiceSettings.ExperimentalGroupUnreadChannels != model.GROUP_UNREAD_CHANNELS_DISABLED {
		props["ExperimentalChannelOrganization"] = strconv.FormatBool(true)
	} else {
		props["ExperimentalChannelOrganization"] = strconv.FormatBool(false)
	}

	props["ExperimentalEnableAutomaticReplies"] = strconv.FormatBool(*c.TeamSettings.ExperimentalEnableAutomaticReplies)
	props["ExperimentalTimezone"] = strconv.FormatBool(*c.DisplaySettings.ExperimentalTimezone)

	props["SendEmailNotifications"] = strconv.FormatBool(*c.EmailSettings.SendEmailNotifications)
	props["SendPushNotifications"] = strconv.FormatBool(*c.EmailSettings.SendPushNotifications)
	props["RequireEmailVerification"] = strconv.FormatBool(*c.EmailSettings.RequireEmailVerification)
	props["EnableEmailBatching"] = strconv.FormatBool(*c.EmailSettings.EnableEmailBatching)
	props["EnablePreviewModeBanner"] = strconv.FormatBool(*c.EmailSettings.EnablePreviewModeBanner)
	props["EmailNotificationContentsType"] = *c.EmailSettings.EmailNotificationContentsType

	props["ShowEmailAddress"] = strconv.FormatBool(*c.PrivacySettings.ShowEmailAddress)

	props["EnableFileAttachments"] = strconv.FormatBool(*c.FileSettings.EnableFileAttachments)
	props["EnablePublicLink"] = strconv.FormatBool(*c.FileSettings.EnablePublicLink)

	props["AvailableLocales"] = *c.LocalizationSettings.AvailableLocales
	props["SQLDriverName"] = *c.SqlSettings.DriverName

	props["EnableEmojiPicker"] = strconv.FormatBool(*c.ServiceSettings.EnableEmojiPicker)
	props["EnableGifPicker"] = strconv.FormatBool(*c.ServiceSettings.EnableGifPicker)
	props["GfycatApiKey"] = *c.ServiceSettings.GfycatApiKey
	props["GfycatApiSecret"] = *c.ServiceSettings.GfycatApiSecret
	props["MaxFileSize"] = strconv.FormatInt(*c.FileSettings.MaxFileSize, 10)

	props["MaxNotificationsPerChannel"] = strconv.FormatInt(*c.TeamSettings.MaxNotificationsPerChannel, 10)
	props["EnableConfirmNotificationsToChannel"] = strconv.FormatBool(*c.TeamSettings.EnableConfirmNotificationsToChannel)
	props["TimeBetweenUserTypingUpdatesMilliseconds"] = strconv.FormatInt(*c.ServiceSettings.TimeBetweenUserTypingUpdatesMilliseconds, 10)
	props["EnableUserTypingMessages"] = strconv.FormatBool(*c.ServiceSettings.EnableUserTypingMessages)
	props["EnableChannelViewedMessages"] = strconv.FormatBool(*c.ServiceSettings.EnableChannelViewedMessages)

	props["RunJobs"] = strconv.FormatBool(*c.JobSettings.RunJobs)

	props["EnableEmailInvitations"] = strconv.FormatBool(*c.ServiceSettings.EnableEmailInvitations)

	// Set default values for all options that require a license.
	props["ExperimentalHideTownSquareinLHS"] = "false"
	props["ExperimentalTownSquareIsReadOnly"] = "false"
	props["ExperimentalEnableAuthenticationTransfer"] = "true"
	props["LdapNicknameAttributeSet"] = "false"
	props["LdapFirstNameAttributeSet"] = "false"
	props["LdapLastNameAttributeSet"] = "false"
	props["EnableCompliance"] = "false"
	props["EnableMobileFileDownload"] = "true"
	props["EnableMobileFileUpload"] = "true"
	props["SamlFirstNameAttributeSet"] = "false"
	props["SamlLastNameAttributeSet"] = "false"
	props["SamlNicknameAttributeSet"] = "false"
	props["EnableCluster"] = "false"
	props["EnableMetrics"] = "false"
	props["PasswordMinimumLength"] = "0"
	props["PasswordRequireLowercase"] = "false"
	props["PasswordRequireUppercase"] = "false"
	props["PasswordRequireNumber"] = "false"
	props["PasswordRequireSymbol"] = "false"
	props["EnableBanner"] = "false"
	props["BannerText"] = ""
	props["BannerColor"] = ""
	props["BannerTextColor"] = ""
	props["AllowBannerDismissal"] = "false"
	props["EnableThemeSelection"] = "true"
	props["DefaultTheme"] = ""
	props["AllowCustomThemes"] = "true"
	props["AllowedThemes"] = ""
	props["DataRetentionEnableMessageDeletion"] = "false"
	props["DataRetentionMessageRetentionDays"] = "0"
	props["DataRetentionEnableFileDeletion"] = "false"
	props["DataRetentionFileRetentionDays"] = "0"
	props["PasswordMinimumLength"] = fmt.Sprintf("%v", *c.PasswordSettings.MinimumLength)
	props["PasswordRequireLowercase"] = strconv.FormatBool(*c.PasswordSettings.Lowercase)
	props["PasswordRequireUppercase"] = strconv.FormatBool(*c.PasswordSettings.Uppercase)
	props["PasswordRequireNumber"] = strconv.FormatBool(*c.PasswordSettings.Number)
	props["PasswordRequireSymbol"] = strconv.FormatBool(*c.PasswordSettings.Symbol)
	props["CustomUrlSchemes"] = strings.Join(c.DisplaySettings.CustomUrlSchemes, ",")

	if license != nil {
		props["ExperimentalHideTownSquareinLHS"] = strconv.FormatBool(*c.TeamSettings.ExperimentalHideTownSquareinLHS)
		props["ExperimentalTownSquareIsReadOnly"] = strconv.FormatBool(*c.TeamSettings.ExperimentalTownSquareIsReadOnly)
		props["ExperimentalEnableAuthenticationTransfer"] = strconv.FormatBool(*c.ServiceSettings.ExperimentalEnableAuthenticationTransfer)

		if *license.Features.LDAP {
			props["LdapNicknameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.NicknameAttribute != "")
			props["LdapFirstNameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.FirstNameAttribute != "")
			props["LdapLastNameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.LastNameAttribute != "")
		}

		if *license.Features.Compliance {
			props["EnableCompliance"] = strconv.FormatBool(*c.ComplianceSettings.Enable)
			props["EnableMobileFileDownload"] = strconv.FormatBool(*c.FileSettings.EnableMobileDownload)
			props["EnableMobileFileUpload"] = strconv.FormatBool(*c.FileSettings.EnableMobileUpload)
		}

		if *license.Features.SAML {
			props["SamlFirstNameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.FirstNameAttribute != "")
			props["SamlLastNameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.LastNameAttribute != "")
			props["SamlNicknameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.NicknameAttribute != "")

			// do this under the correct licensed feature
			props["ExperimentalClientSideCertEnable"] = strconv.FormatBool(*c.ExperimentalSettings.ClientSideCertEnable)
			props["ExperimentalClientSideCertCheck"] = *c.ExperimentalSettings.ClientSideCertCheck
		}

		if *license.Features.Cluster {
			props["EnableCluster"] = strconv.FormatBool(*c.ClusterSettings.Enable)
		}

		if *license.Features.Cluster {
			props["EnableMetrics"] = strconv.FormatBool(*c.MetricsSettings.Enable)
		}

		if *license.Features.Announcement {
			props["EnableBanner"] = strconv.FormatBool(*c.AnnouncementSettings.EnableBanner)
			props["BannerText"] = *c.AnnouncementSettings.BannerText
			props["BannerColor"] = *c.AnnouncementSettings.BannerColor
			props["BannerTextColor"] = *c.AnnouncementSettings.BannerTextColor
			props["AllowBannerDismissal"] = strconv.FormatBool(*c.AnnouncementSettings.AllowBannerDismissal)
		}

		if *license.Features.ThemeManagement {
			props["EnableThemeSelection"] = strconv.FormatBool(*c.ThemeSettings.EnableThemeSelection)
			props["DefaultTheme"] = *c.ThemeSettings.DefaultTheme
			props["AllowCustomThemes"] = strconv.FormatBool(*c.ThemeSettings.AllowCustomThemes)
			props["AllowedThemes"] = strings.Join(c.ThemeSettings.AllowedThemes, ",")
		}

		if *license.Features.DataRetention {
			props["DataRetentionEnableMessageDeletion"] = strconv.FormatBool(*c.DataRetentionSettings.EnableMessageDeletion)
			props["DataRetentionMessageRetentionDays"] = strconv.FormatInt(int64(*c.DataRetentionSettings.MessageRetentionDays), 10)
			props["DataRetentionEnableFileDeletion"] = strconv.FormatBool(*c.DataRetentionSettings.EnableFileDeletion)
			props["DataRetentionFileRetentionDays"] = strconv.FormatInt(int64(*c.DataRetentionSettings.FileRetentionDays), 10)
		}
	}

	return props
}

func GenerateLimitedClientConfig(c *model.Config, diagnosticId string, license *model.License) map[string]string {
	props := make(map[string]string)

	props["Version"] = model.CurrentVersion
	props["BuildNumber"] = model.BuildNumber
	props["BuildDate"] = model.BuildDate
	props["BuildHash"] = model.BuildHash
	props["BuildHashEnterprise"] = model.BuildHashEnterprise
	props["BuildEnterpriseReady"] = model.BuildEnterpriseReady

	props["SiteName"] = *c.TeamSettings.SiteName
	props["WebsocketURL"] = strings.TrimRight(*c.ServiceSettings.WebsocketURL, "/")
	props["WebsocketPort"] = fmt.Sprintf("%v", *c.ServiceSettings.WebsocketPort)
	props["WebsocketSecurePort"] = fmt.Sprintf("%v", *c.ServiceSettings.WebsocketSecurePort)
	props["EnableUserCreation"] = strconv.FormatBool(*c.TeamSettings.EnableUserCreation)
	props["EnableOpenServer"] = strconv.FormatBool(*c.TeamSettings.EnableOpenServer)

	props["AndroidLatestVersion"] = c.ClientRequirements.AndroidLatestVersion
	props["AndroidMinVersion"] = c.ClientRequirements.AndroidMinVersion
	props["DesktopLatestVersion"] = c.ClientRequirements.DesktopLatestVersion
	props["DesktopMinVersion"] = c.ClientRequirements.DesktopMinVersion
	props["IosLatestVersion"] = c.ClientRequirements.IosLatestVersion
	props["IosMinVersion"] = c.ClientRequirements.IosMinVersion

	props["EnableDiagnostics"] = strconv.FormatBool(*c.LogSettings.EnableDiagnostics)

	props["EnableSignUpWithEmail"] = strconv.FormatBool(*c.EmailSettings.EnableSignUpWithEmail)
	props["EnableSignInWithEmail"] = strconv.FormatBool(*c.EmailSettings.EnableSignInWithEmail)
	props["EnableSignInWithUsername"] = strconv.FormatBool(*c.EmailSettings.EnableSignInWithUsername)

	props["EmailLoginButtonColor"] = *c.EmailSettings.LoginButtonColor
	props["EmailLoginButtonBorderColor"] = *c.EmailSettings.LoginButtonBorderColor
	props["EmailLoginButtonTextColor"] = *c.EmailSettings.LoginButtonTextColor

	props["EnableSignUpWithGitLab"] = strconv.FormatBool(*c.GitLabSettings.Enable)

	props["TermsOfServiceLink"] = *c.SupportSettings.TermsOfServiceLink
	props["PrivacyPolicyLink"] = *c.SupportSettings.PrivacyPolicyLink
	props["AboutLink"] = *c.SupportSettings.AboutLink
	props["HelpLink"] = *c.SupportSettings.HelpLink
	props["ReportAProblemLink"] = *c.SupportSettings.ReportAProblemLink
	props["SupportEmail"] = *c.SupportSettings.SupportEmail

	props["DefaultClientLocale"] = *c.LocalizationSettings.DefaultClientLocale

	props["EnableCustomEmoji"] = strconv.FormatBool(*c.ServiceSettings.EnableCustomEmoji)
	props["AppDownloadLink"] = *c.NativeAppSettings.AppDownloadLink
	props["AndroidAppDownloadLink"] = *c.NativeAppSettings.AndroidAppDownloadLink
	props["IosAppDownloadLink"] = *c.NativeAppSettings.IosAppDownloadLink

	props["DiagnosticId"] = diagnosticId
	props["DiagnosticsEnabled"] = strconv.FormatBool(*c.LogSettings.EnableDiagnostics)

	props["HasImageProxy"] = strconv.FormatBool(*c.ImageProxySettings.Enable)

	props["PluginsEnabled"] = strconv.FormatBool(*c.PluginSettings.Enable)

	// Set default values for all options that require a license.
	props["EnableCustomBrand"] = "false"
	props["CustomBrandText"] = ""
	props["CustomDescriptionText"] = ""
	props["EnableLdap"] = "false"
	props["LdapLoginFieldName"] = ""
	props["LdapLoginButtonColor"] = ""
	props["LdapLoginButtonBorderColor"] = ""
	props["LdapLoginButtonTextColor"] = ""
	props["EnableSaml"] = "false"
	props["SamlLoginButtonText"] = ""
	props["SamlLoginButtonColor"] = ""
	props["SamlLoginButtonBorderColor"] = ""
	props["SamlLoginButtonTextColor"] = ""
	props["EnableSignUpWithGoogle"] = "false"
	props["EnableSignUpWithOffice365"] = "false"
	props["EnableCustomBrand"] = strconv.FormatBool(*c.TeamSettings.EnableCustomBrand)
	props["CustomBrandText"] = *c.TeamSettings.CustomBrandText
	props["CustomDescriptionText"] = *c.TeamSettings.CustomDescriptionText
	props["EnableMultifactorAuthentication"] = strconv.FormatBool(*c.ServiceSettings.EnableMultifactorAuthentication)
	props["EnforceMultifactorAuthentication"] = "false"

	if license != nil {
		if *license.Features.LDAP {
			props["EnableLdap"] = strconv.FormatBool(*c.LdapSettings.Enable)
			props["LdapLoginFieldName"] = *c.LdapSettings.LoginFieldName
			props["LdapLoginButtonColor"] = *c.LdapSettings.LoginButtonColor
			props["LdapLoginButtonBorderColor"] = *c.LdapSettings.LoginButtonBorderColor
			props["LdapLoginButtonTextColor"] = *c.LdapSettings.LoginButtonTextColor
		}

		if *license.Features.SAML {
			props["EnableSaml"] = strconv.FormatBool(*c.SamlSettings.Enable)
			props["SamlLoginButtonText"] = *c.SamlSettings.LoginButtonText
			props["SamlLoginButtonColor"] = *c.SamlSettings.LoginButtonColor
			props["SamlLoginButtonBorderColor"] = *c.SamlSettings.LoginButtonBorderColor
			props["SamlLoginButtonTextColor"] = *c.SamlSettings.LoginButtonTextColor
		}

		if *license.Features.GoogleOAuth {
			props["EnableSignUpWithGoogle"] = strconv.FormatBool(*c.GoogleSettings.Enable)
		}

		if *license.Features.Office365OAuth {
			props["EnableSignUpWithOffice365"] = strconv.FormatBool(*c.Office365Settings.Enable)
		}

		if *license.Features.CustomTermsOfService {
			props["EnableCustomTermsOfService"] = strconv.FormatBool(*c.SupportSettings.CustomTermsOfServiceEnabled)
			props["CustomTermsOfServiceReAcceptancePeriod"] = strconv.FormatInt(int64(*c.SupportSettings.CustomTermsOfServiceReAcceptancePeriod), 10)
		}

		if *license.Features.MFA {
			props["EnforceMultifactorAuthentication"] = strconv.FormatBool(*c.ServiceSettings.EnforceMultifactorAuthentication)
		}
	}

	return props
}

func ValidateLocales(cfg *model.Config) *model.AppError {
	var err *model.AppError
	locales := utils.GetSupportedLocales()
	if _, ok := locales[*cfg.LocalizationSettings.DefaultServerLocale]; !ok {
		*cfg.LocalizationSettings.DefaultServerLocale = model.DEFAULT_LOCALE
		err = model.NewAppError("ValidateLocales", "utils.config.supported_server_locale.app_error", nil, "", http.StatusBadRequest)
	}

	if _, ok := locales[*cfg.LocalizationSettings.DefaultClientLocale]; !ok {
		*cfg.LocalizationSettings.DefaultClientLocale = model.DEFAULT_LOCALE
		err = model.NewAppError("ValidateLocales", "utils.config.supported_client_locale.app_error", nil, "", http.StatusBadRequest)
	}

	if len(*cfg.LocalizationSettings.AvailableLocales) > 0 {
		isDefaultClientLocaleInAvailableLocales := false
		for _, word := range strings.Split(*cfg.LocalizationSettings.AvailableLocales, ",") {
			if _, ok := locales[word]; !ok {
				*cfg.LocalizationSettings.AvailableLocales = ""
				isDefaultClientLocaleInAvailableLocales = true
				err = model.NewAppError("ValidateLocales", "utils.config.supported_available_locales.app_error", nil, "", http.StatusBadRequest)
				break
			}

			if word == *cfg.LocalizationSettings.DefaultClientLocale {
				isDefaultClientLocaleInAvailableLocales = true
			}
		}

		availableLocales := *cfg.LocalizationSettings.AvailableLocales

		if !isDefaultClientLocaleInAvailableLocales {
			availableLocales += "," + *cfg.LocalizationSettings.DefaultClientLocale
			err = model.NewAppError("ValidateLocales", "utils.config.add_client_locale.app_error", nil, "", http.StatusBadRequest)
		}

		*cfg.LocalizationSettings.AvailableLocales = strings.Join(utils.RemoveDuplicatesFromStringArray(strings.Split(availableLocales, ",")), ",")
	}

	return err
}
