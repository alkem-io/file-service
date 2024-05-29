import { WINSTON_MODULE_NEST_PROVIDER } from 'nest-winston';
import { NestFactory } from '@nestjs/core';
import {
  FastifyAdapter,
  NestFastifyApplication,
} from '@nestjs/platform-fastify';
import fastifyCookie from '@fastify/cookie';
import { ConfigService } from '@nestjs/config';
import { AppModule } from './app.module';
import { ConfigType } from './config';

(async () => {
  const app = await NestFactory.create<NestFastifyApplication>(
    AppModule,
    new FastifyAdapter(),
    {
      /***
       * if the logger is provided at a later stage via 'useLogger' after the app has initialized, Nest falls back to the default logger
       * while initializing, which logs a lot of info logs, which we don't have control over and don't want tracked.
       * The logger is disabled while the app is loading ONLY on production to avoid the messages;
       * then the costume logger is applied as usual
       */
      logger: process.env.NODE_ENV === 'production' ? false : undefined,
    },
  );
  const logger = app.get(WINSTON_MODULE_NEST_PROVIDER);
  app.useLogger(logger);

  await app.register(fastifyCookie);

  const configService: ConfigService<ConfigType, true> = app.get(ConfigService);
  const port = configService.get('settings.application.port', { infer: true });

  await app.listen(port);
})();
