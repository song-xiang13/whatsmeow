await (async () => {
  const SiteData = require("SiteData");
  const WAWebBuildConstants = require("WAWebBuildConstants");
  const WAWebUA = require("WAWebUA");
  const CurrentLocale = require("CurrentLocale");
  const WebBloksHasteUtils = require("WebBloksHasteUtils");
  const UserAgentData = require("UserAgentData");
  const WAWebBackendApi = require("WAWebBackendApi");
  const WAWebClientPayload = require("WAWebClientPayload");
  const WAWebSignalStoreApi = require("WAWebSignalStoreApi");
  const decodeProtobuf = require("decodeProtobuf");
  const WAWebProtobufsWa6 = require("WAWebProtobufsWa6.pb");
  const WAWebProtobufsCompanionReg = require("WAWebProtobufsCompanionReg.pb");

  const ua = WAWebUA.UA || WAWebUA.parseUA(navigator.userAgent);
  const currentLocaleCode = CurrentLocale.get();
  const webLocale = WebBloksHasteUtils.getLocaleFromServer();

  const localeParts = String(currentLocaleCode || "").split(/[_-]/);
  const localeLanguageIso6391 = localeParts[0]
    ? localeParts[0].toLowerCase()
    : null;
  const localeCountryIso31661Alpha2 = localeParts[1]
    ? localeParts[1].toUpperCase()
    : null;

  let deviceInfo = null;
  deviceInfo = await WAWebBackendApi.frontendSendAndReceive("getDeviceInfo", void 0);

  let devicePropsVersion = null;
  try {
    const [registrationInfo, signedPreKey] = await Promise.all([
      WAWebSignalStoreApi.waSignalStore.getRegistrationInfo(),
      WAWebSignalStoreApi.waSignalStore.getSignedPreKey(),
    ]);

    if (registrationInfo && signedPreKey) {
      const payloadBytes = await WAWebClientPayload.getClientPayloadForRegistration(
        registrationInfo,
        signedPreKey,
        { passive: false, pull: false }
      );
      const clientPayload = decodeProtobuf.decodeProtobuf(
        WAWebProtobufsWa6.ClientPayloadSpec,
        payloadBytes
      );
      const devicePropsBytes =
        clientPayload?.devicePairingData?.deviceProps ?? null;
      const deviceProps = devicePropsBytes
        ? decodeProtobuf.decodeProtobuf(
            WAWebProtobufsCompanionReg.DevicePropsSpec,
            devicePropsBytes
          )
        : null;

      devicePropsVersion = deviceProps?.version ?? null;
    }
  } catch (e) {
    devicePropsVersion = null;
  }

  const result = {
    version: {
      client_revision: SiteData.client_revision,
      push_phase: SiteData.push_phase,
      VERSION_PRIMARY: WAWebBuildConstants.VERSION_PRIMARY,
      VERSION_SECONDARY: WAWebBuildConstants.VERSION_SECONDARY,
      VERSION_TERTIARY: WAWebBuildConstants.VERSION_TERTIARY,
      VERSION_BASE: WAWebBuildConstants.VERSION_BASE,
      VERSION_BASE_WITH_WINDOWS_BUILD:
        WAWebBuildConstants.VERSION_BASE_WITH_WINDOWS_BUILD,
      VERSION_STR: WAWebBuildConstants.VERSION_STR,
    },

    ua: {
      os: ua.os,
      osVersion: ua.osVersion,
      browser: ua.browser,
      browserVersion: ua.browserVersion,
      isChrome: ua.isChrome,
      isFirefox: ua.isFirefox,
      isSafari: ua.isSafari,
      isWebkit: ua.isWebkit,
      isGecko: ua.isGecko,
      isBlink: ua.isBlink,
    },

    userAgentData: {
      browserName: UserAgentData.browserName,
      browserFullVersion: UserAgentData.browserFullVersion,
      browserArchitecture: UserAgentData.browserArchitecture,
      engineName: UserAgentData.engineName,
      engineVersion: UserAgentData.engineVersion,
      platformName: UserAgentData.platformName,
      platformFullVersion: UserAgentData.platformFullVersion,
      platformArchitecture: UserAgentData.platformArchitecture,
      deviceName: UserAgentData.deviceName,
    },

    locale: {
      currentLocaleCode,
      webLocale,
      localeLanguageIso6391,
      localeCountryIso31661Alpha2,
    },
    devicePropsVersion,

    deviceInfoFromBackend: deviceInfo
      ? {
          raw: deviceInfo,
          languageCode: deviceInfo.lg ?? null,
          locationCode: deviceInfo.lc ?? null,
          osBuildNumber: deviceInfo.osBuild ?? null,
          mcc: deviceInfo.mcc ?? null,
          mnc: deviceInfo.mnc ?? null,
        }
      : null,
  };
  return result;
})();

