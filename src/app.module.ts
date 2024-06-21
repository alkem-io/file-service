import { WinstonModule } from 'nest-winston';
import { Module } from '@nestjs/common';
import { ConfigModule } from '@nestjs/config';
import { WinstonConfigService } from './config';
import configuration from './config/configuration';
import { FileModule } from './services/file-reader';
import { BaseExceptionFilterProvider } from './core/filters';
import { HealthModule } from './services/health';

@Module({
  imports: [
    ConfigModule.forRoot({
      envFilePath: ['.env'],
      isGlobal: true,
      load: [configuration],
    }),
    WinstonModule.forRootAsync({
      useClass: WinstonConfigService,
    }),
    FileModule,
    HealthModule,
  ],
  providers: [BaseExceptionFilterProvider],
})
export class AppModule {}
