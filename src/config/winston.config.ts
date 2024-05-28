import { Injectable } from '@nestjs/common';
import * as winston from 'winston';
import { utilities as nestWinstonModuleUtilities } from 'nest-winston';
import * as logform from 'logform';
import { ConfigService } from '@nestjs/config';
import { ConfigType } from './config.type';

const LOG_LABEL = 'alkemio-whiteboard-collaboration';

const consoleLoggingStandardFormat: logform.Format[] = [
  winston.format.timestamp(),
  nestWinstonModuleUtilities.format.nestLike(undefined, {
    colors: true,
    prettyPrint: true,
  }),
];

const consoleLoggingProdFormat: logform.Format[] = [
  winston.format.timestamp(),
  winston.format.label({ label: LOG_LABEL }),
  winston.format.json({ deterministic: true }),
];

@Injectable()
export class WinstonConfigService {
  constructor(
    private readonly configService: ConfigService<ConfigType, true>,
  ) {}

  createWinstonModuleOptions() {
    const { enabled, level, json } = this.configService.get(
      'monitoring.logging',
      { infer: true },
    );
    const transports: any[] = [
      new winston.transports.Console({
        format: winston.format.combine(
          ...(json ? consoleLoggingProdFormat : consoleLoggingStandardFormat),
        ),
        level,
        silent: !enabled,
      }),
    ];

    return {
      transports: transports,
    };
  }
}
